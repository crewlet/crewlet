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
import {
  Button,
  Checkbox,
  Combobox,
  FormField,
  Input,
  Modal,
  Select,
  Textarea,
  type ComboboxOption,
  type SelectOption,
} from "@crewlethq/ui";
import {
  UndoGlyph,
  PinGlyph,
  CornerDownLeftGlyph,
  MessageSquareGlyph,
  UserPlusGlyph,
  PencilGlyph,
  PlusGlyph,
  PauseGlyph,
  RotateCwGlyph,
  SearchGlyph,
  CompassGlyph,
} from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton, pressable } from "./WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { handleLabel, indexOrg, nameOfIn } from "~/lib/seats.ts";
import { targetLabel } from "~/lib/work.ts";
import { useOpenNewTask } from "~/app/newTask.ts";
import { STEER_NOTE_MAX_RUNES } from "~/contract/steer.ts";

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
  const blocked = who === assignee ? "Choose somebody other than who holds it now." : undefined;
  const submit = async () => {
    // THE BUTTON'S OWN GATE, because the dialog is a form Enter submits.
    if (!pressable(write, blocked)) return;
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
            blocked={blocked}
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
 * "Edit project": the lead's target date, set or cleared.
 *
 * A COMPANY WRITE (`contract/actions.ts` scope `company`): a project's
 * settings are what every seat's tracker reads, so it is offered on the
 * project's own page and nowhere else. WHO MAY is the engine's rule, not this
 * control's — the project's lead, or a person acting as themselves — and a
 * refusal says so in the engine's sentence.
 *
 * ONE FIELD, because it is the one project setting that is a person's
 * judgement rather than the company configuration's: a project's name,
 * purpose, unit and lead come from the org chart and are edited there. An
 * empty date CLEARS the target, which the tool reads as `null`; the button is
 * blocked while the date is the one already set, so a press always changes
 * something.
 *
 * THE PROJECT IS CALLED BY ITS NAME, in the dialog's title and in the toast
 * that confirms it — the key is what a person types, the name is what the
 * page they pressed this on is headed with — and the day is written the way
 * the Target fact on that page writes it ([targetLabel]), not as the wire's
 * `2026-10-30`.
 */
export function EditProjectButton({
  project,
  name,
  target,
  open: held,
  onOpenChange,
}: {
  project: string;
  /** The project's name, as its page is headed; the key when it has none. */
  name?: string;
  target?: string;
  /**
   * Whether the dialog is open, for a page that also opens it from somewhere
   * other than this button — the page bar's "More" on a phone, where the
   * button itself is folded away. Left out, the button holds it.
   */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  const write = useAct("write_project");
  const [own, setOwn] = useState(false);
  const open = held ?? own;
  const setOpen = (next: boolean) => {
    setOwn(next);
    onOpenChange?.(next);
  };
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        leadingIcon={<PencilGlyph />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        Edit project
      </WriteButton>
      {open && (
        <EditProjectDialog
          project={project}
          name={name || project}
          target={target ?? ""}
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

function EditProjectDialog({
  project,
  name,
  target,
  write,
  onClose,
}: {
  project: string;
  name: string;
  target: string;
  write: ReturnType<typeof useAct<"write_project">>;
  onClose: () => void;
}) {
  const [day, setDay] = useState(target);
  const blocked = day === target ? "Choose a different date, or clear the one set." : undefined;
  const submit = async () => {
    // THE BUTTON'S OWN GATE: the dialog is a form whose one date field
    // submits it on Enter, and a second Enter while the first answer was out
    // wrote the same day again.
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { project, target_date: day || null },
      {
        done: day ? `Set ${name}'s target to ${targetLabel(day)}` : `Cleared ${name}'s target date`,
      },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Edit ${name}`}
      subtitle={name === project ? undefined : project}
      icon={<PencilGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      dismissable={!write.busy}
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
            blocked={blocked}
          >
            Save
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Target date"
          optional
          htmlFor="project-target"
          helper="The day the project is meant to be finished, on the company's clock. Empty clears it."
        >
          <div className="row gap-2">
            <Input
              id="project-target"
              type="date"
              value={day}
              onChange={(event) => setDay(event.target.value)}
            />
            {day && (
              <Button
                size="small"
                variant="ghost"
                // WHAT IT CLEARS, for a reader who reaches it by name.
                aria-label="Clear target date"
                onClick={() => setDay("")}
              >
                Clear
              </Button>
            )}
          </div>
        </FormField>
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * "+ View": the query on screen, saved under a name on the container it is
 * about.
 *
 * WHAT IS SAVED IS THE QUESTION, in the grammar's own keys — the filters, the
 * scope, the grouping and the order — and the SHAPE as the view's type, so
 * the saved view opens drawn the way it was saved. Paging and a shape's own
 * window are not saved (the caller strips them): a view that carried the
 * calendar's month would open on that month for ever.
 *
 * SHARED unless the person keeps it to themselves, which makes it theirs alone
 * (`owner`): a shared view is in everybody's strip, and saving somebody's
 * private query into it is a change to their screen too.
 *
 * On `applied` it hands the new view's id back, so the screen opens it.
 */
export function SaveViewButton({
  container,
  type,
  params,
  onSaved,
}: {
  /** `workspace` or `project:<KEY>`. */
  container: string;
  /** The shape the view opens as. */
  type: string;
  /** The query, as the grammar spells it. */
  params: Record<string, string>;
  /** The saved view's id, once the engine has it. */
  onSaved?: (id: string) => void;
}) {
  const write = useAct("save_work_view");
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="ghost"
        leadingIcon={<PlusGlyph />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        View
      </WriteButton>
      {open && (
        <SaveViewDialog
          container={container}
          type={type}
          params={params}
          write={write}
          onClose={(id) => {
            write.dismiss();
            setOpen(false);
            if (id) onSaved?.(id);
          }}
        />
      )}
    </>
  );
}

function SaveViewDialog({
  container,
  type,
  params,
  write,
  onClose,
}: {
  container: string;
  type: string;
  params: Record<string, string>;
  write: ReturnType<typeof useAct<"save_work_view">>;
  onClose: (id?: string) => void;
}) {
  const [name, setName] = useState("");
  const [mine, setMine] = useState(false);
  const as = write.access.can ? write.access.as : "";
  const blocked = name.trim() ? undefined : "Give the view a name first.";
  const submit = async () => {
    // THE BUTTON'S OWN GATE, because the dialog is a form its one field
    // submits on Enter: a second Enter saved a second view.
    if (!pressable(write, blocked)) return;
    const title = name.trim();
    const result = await write.run(
      { container, name: title, type, params, ...(mine && as ? { owner: as } : {}) },
      { done: `Saved the view ${title}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      const id = (result.receipt as { id?: unknown } | null)?.id;
      onClose(typeof id === "string" ? id : undefined);
    }
  };
  return (
    <Modal
      open
      size="sm"
      title="Save this view"
      icon={<PinGlyph />}
      onClose={() => onClose()}
      onSubmit={() => void submit()}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={() => onClose()} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            onPress={() => void submit()}
            blocked={blocked}
          >
            Save view
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Name"
          htmlFor="save-view-name"
          helper="The tab's label, in the strip above the work."
        >
          <Input
            id="save-view-name"
            value={name}
            maxLength={80}
            onChange={(event) => setName(event.target.value)}
            autoFocus
          />
        </FormField>
        <Checkbox
          checked={mine}
          onChange={(event) => setMine(event.target.checked)}
          label="Only for me"
          description="Kept in your own strip. Otherwise everybody who opens this list sees it."
        />
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * Answer a structured ask with one of its options — the decision, made in
 * place.
 *
 * THE CHOICE IS AN ANSWER TO THE ASK IT BELONGS TO (`answers` names the ask's
 * comment), so the engine checks the option is one the ask offered, closes the
 * ask, and wakes the asker with what was chosen. The recommended option is
 * the primary button: it is the one most answers are, and the asker said so.
 *
 * ONE WRITE FOR THE WHOLE ASK, never one per option. An ask takes one answer,
 * and with a write per button the other options stayed pressable while the
 * first was in flight — a second, different choice sent to the same ask. So
 * every option sends through the one `useAct`: the pressed one shows it is
 * sending, the rest are disabled with the reason until it settles, and the
 * refusal, if there is one, is drawn once for the ask.
 */
export function AnswerAskButtons({
  item,
  comment,
  options,
  recommended,
}: {
  /** The task's key or id. */
  item: string;
  /** The ask's comment id. */
  comment: string;
  /** The ask's options, whose words are the buttons'. */
  options: readonly { id: string; label: string }[];
  /** The option the asker recommends, if any. */
  recommended?: string;
}) {
  const write = useAct("comment_on_work_item");
  const [pressed, setPressed] = useState<string | null>(null);
  return (
    <>
      {options.map((option) => {
        const mine = write.busy && pressed === option.id;
        return (
          <WriteButton
            key={option.id}
            write={write}
            size="small"
            variant={option.id === recommended ? "primary" : "secondary"}
            pressing={mine}
            blocked={write.busy && !mine ? "Your answer to this ask is being sent" : undefined}
            showRefusal={false}
            onPress={() => {
              setPressed(option.id);
              void write.run(
                { item, answers: comment, choice: option.id },
                { done: `Answered ${item}: ${option.label}` },
              );
            }}
          >
            {option.label}
          </WriteButton>
        );
      })}
      <RefusalNote write={write} />
    </>
  );
}

/**
 * Answer an ask that offers no options — in words, in place.
 *
 * THE SAME ANSWER AN OPTION SENDS, WITH A BODY INSTEAD OF A CHOICE:
 * `comment_on_work_item{item, answers, body}` names the ask's comment, so the
 * engine closes that ask and wakes the asker with the reply. The dialog is the
 * one `AnswerRunButton` opens, because the gesture is the same one — read the
 * question, write the answer, send it as yourself — and a second shape for it
 * would be a second thing to learn.
 */
export function ReplyAskButton({
  item,
  comment,
  asker,
  question,
}: {
  /** The task's key or id. */
  item: string;
  /** The ask's comment id, which the reply answers. */
  comment: string;
  /** Who asked, by name, for the dialog's title. */
  asker: string;
  /** What they asked, quoted above the answer. */
  question: string;
}) {
  const write = useAct("comment_on_work_item");
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  const close = () => {
    write.dismiss();
    setOpen(false);
  };
  const blocked = text.trim() ? undefined : "Write the reply first.";
  const submit = async () => {
    // THE BUTTON'S OWN GATE, for the dialog's submit: a reply is a comment,
    // and a second one is not the first sent again.
    if (!pressable(write, blocked)) return;
    const body = text.trim();
    const result = await write.run(
      { item, answers: comment, body },
      { done: `Replied to ${asker} on ${item}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setText("");
      close();
    }
  };
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        leadingIcon={<MessageSquareGlyph />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        Reply
      </WriteButton>
      {open && (
        <Modal
          open
          size="md"
          title={`Reply to ${asker}`}
          icon={<MessageSquareGlyph />}
          onClose={close}
          onSubmit={() => void submit()}
          closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
          footer={
            <>
              <Button variant="secondary" onClick={close} disabled={write.busy}>
                Cancel
              </Button>
              <WriteButton
                write={write}
                variant="primary"
                showRefusal={false}
                onPress={() => void submit()}
                blocked={blocked}
              >
                Send reply
              </WriteButton>
            </>
          }
        >
          <div className="col gap-3">
            {question && <blockquote className="answer-run-question">{question}</blockquote>}
            <FormField
              label="Your reply"
              htmlFor={`reply-ask-${comment}`}
              helper={`Posted on ${item} as the answer to this question; ${asker} is woken with it.`}
            >
              <Textarea
                id={`reply-ask-${comment}`}
                rows={5}
                value={text}
                onChange={(event) => setText(event.target.value)}
              />
            </FormField>
            <RefusalNote write={write} />
          </div>
        </Modal>
      )}
    </>
  );
}

/**
 * Answer a coding run that stopped to ask a person something, by its turn —
 * which reaches a run whatever woke the turn that launched it, chat thread or
 * none.
 *
 * `pending` IS THE ORDINARY ANSWER: the answer is on the seat's inbox and the
 * node holding the seat resumes the run with it, which the run's own record
 * says once it has.
 */
export function AnswerRunButton({
  turnId,
  seat,
  question,
}: {
  turnId: string;
  /** Whose run it is, by name, for the dialog's title. */
  seat: string;
  /** What the run asked, quoted above the answer. */
  question: string;
}) {
  const write = useAct("answer_run");
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        leadingIcon={<CornerDownLeftGlyph />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        Answer
      </WriteButton>
      {open && (
        <AnswerRunDialog
          turnId={turnId}
          seat={seat}
          question={question}
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

function AnswerRunDialog({
  turnId,
  seat,
  question,
  write,
  onClose,
}: {
  turnId: string;
  seat: string;
  question: string;
  write: ReturnType<typeof useAct<"answer_run">>;
  onClose: () => void;
}) {
  const [answer, setAnswer] = useState("");
  const blocked = answer.trim() ? undefined : "Write the answer first.";
  const submit = async () => {
    // THE BUTTON'S OWN GATE, for the dialog's submit.
    if (!pressable(write, blocked)) return;
    const text = answer.trim();
    const result = await write.run(
      { turn_id: turnId, answer: text },
      { done: `Answered ${seat}'s coding run` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="md"
      title={`Answer ${seat}'s coding run`}
      icon={<CornerDownLeftGlyph />}
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
            blocked={blocked}
          >
            Send answer
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        {question && <blockquote className="answer-run-question">{question}</blockquote>}
        <FormField
          label="Your answer"
          htmlFor="answer-run-text"
          helper="The run resumes with this as the answer to its question, as the coding agent reads it."
        >
          <Textarea
            id="answer-run-text"
            rows={5}
            value={answer}
            onChange={(event) => setAnswer(event.target.value)}
          />
        </FormField>
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// A seat's own controls: message it, give it work, pause it
// ---------------------------------------------------------------------------

/**
 * Message a seat — as a task that ASKS it.
 *
 * THERE IS NO PERSON-TO-SEAT CHAT CHANNEL. Message opens the one New task
 * sheet with the seat as assignee and `ask` naming it, so the question is
 * filed as work the seat owes an answer on, and the answer lands in the
 * asker's Inbox. The sheet's Create is the write; this button opens it, and is
 * held with the reason where the reader could not file one — a door that
 * opened onto a sheet that then refused would teach the reason one press too
 * late.
 */
export function MessageSeatButton({
  handle,
  variant = "secondary",
}: {
  handle: string;
  variant?: "primary" | "secondary";
}) {
  const write = useAct("create_work_item");
  const openNewTask = useOpenNewTask();
  return (
    <WriteButton
      write={write}
      size="small"
      variant={variant}
      leadingIcon={<MessageSquareGlyph size="sm" />}
      showRefusal={false}
      onPress={() => openNewTask({ assignee: handle, ask: handle })}
    >
      Message
    </WriteButton>
  );
}

/** How many tasks the assign dialog lists for a term: a screenful. */
const ASSIGN_MATCHES = 8;

/** How many matches the assign dialog ASKS for, of which it lists
 *  [ASSIGN_MATCHES]: the engine's own default search page (`queries.
 *  DefaultSearchLimit`, 25). Wider than the list because the list puts the
 *  tasks that can be handed over FIRST — asked for eight, a term like "retry"
 *  came back with seven the seat already held, and the one task that could
 *  move was the only choice while others sat just past the page. */
const ASSIGN_SEARCH = 25;

/**
 * The assign dialog's matches in the order it lists them: the tasks this seat
 * does NOT hold first, in the search's own order, then the seat's own, drawn
 * but DISABLED and saying so — seen, so "why is ENG-26 not here" has an
 * answer, and never chosen, since the only thing choosing one did was say it
 * was already theirs. Capped at [ASSIGN_MATCHES].
 *
 * WHOSE A HIT IS, here, is the tracker's row as of the SEARCH — the engine
 * reads each ranked hit's assignee from the replicated rows the task's own
 * read comes from, so it is current when the list is drawn and stale only by
 * how long the list has been open. That is why the dialog still decides on
 * the task's READ once one is chosen; a task handed away from the seat after
 * the list was drawn becomes choosable when the term is next searched.
 */
function assignOptions(
  hits: readonly { key: string; title: string; assignee?: string }[],
  handle: string,
  nameOf: (handle: string) => string,
): ComboboxOption[] {
  const movable = hits.filter((h) => (h.assignee ?? "") !== handle);
  const theirs = hits.filter((h) => (h.assignee ?? "") === handle);
  return [...movable, ...theirs].slice(0, ASSIGN_MATCHES).map((hit) => {
    const own = (hit.assignee ?? "") === handle;
    return {
      value: hit.key,
      label: `${hit.key} · ${hit.title}`,
      hint: own ? "already theirs" : hit.assignee ? `with ${nameOf(hit.assignee)}` : "unassigned",
      ...(own ? { disabled: true } : {}),
    };
  });
}

/** The shortest term the assign dialog searches on, as the palette's tasks do. */
const ASSIGN_MIN_TERM = 2;

/**
 * Give a seat a task: one that exists, found by search, or a new one.
 *
 * AN EXISTING TASK IS HANDED OVER CONDITIONALLY. The search hit says what the
 * task is, not the version it is at, so the chosen task is READ (`work_item`)
 * and the hand-off is made against the version that read returned
 * (`if_match`) — the same rule the task page's own Assign keeps: a task
 * somebody moved in between is refused "changed since you opened it" rather
 * than taken from whoever holds it now without anybody seeing that they did.
 *
 * A NEW ONE is the New task sheet with the seat preset as its assignee, whose
 * Create is its own write.
 */
export function AssignToSeatButton({
  handle,
  name,
  open: held,
  onOpenChange,
}: {
  handle: string;
  name: string;
  /**
   * Whether the dialog is open, for a page that also opens it from its bar's
   * "More" on a phone, where this button is folded away — as
   * [EditProjectButton] takes it. Left out, the button holds it.
   */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  const write = useAct("update_work_item");
  const [own, setOwn] = useState(false);
  const open = held ?? own;
  const setOpen = (next: boolean) => {
    setOwn(next);
    onOpenChange?.(next);
  };
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        leadingIcon={<PlusGlyph size="sm" />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        Assign task
      </WriteButton>
      {open && (
        <AssignToSeatDialog
          handle={handle}
          name={name}
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

function AssignToSeatDialog({
  handle,
  name,
  write,
  onClose,
}: {
  handle: string;
  name: string;
  write: ReturnType<typeof useAct<"update_work_item">>;
  onClose: () => void;
}) {
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  const openNewTask = useOpenNewTask();
  const [term, setTerm] = useState("");
  const [listOpen, setListOpen] = useState(false);
  const [chosen, setChosen] = useState<{ key: string; title: string; assignee: string } | null>(
    null,
  );
  const [reason, setReason] = useState("");
  const q = term.trim();
  const search = useQuery(
    "work_search",
    { q, mode: "hybrid", limit: ASSIGN_SEARCH },
    { enabled: q.length >= ASSIGN_MIN_TERM },
  );
  // THE VERSION THE HAND-OFF IS CONDITIONAL ON, read for the task chosen.
  const read = useQuery("work_item", chosen ? { id: chosen.key } : undefined, {
    enabled: chosen !== null,
  });
  const hits = q.length >= ASSIGN_MIN_TERM ? (search.data?.hits ?? []) : [];
  const options = useMemo(() => assignOptions(hits, handle, nameOf), [hits, handle, nameOf]);
  const task = read.data && read.data.task.key === chosen?.key ? read.data.task : null;
  const version = task ? task.version : null;
  // WHOSE IT IS NOW is the READ's, once it lands — the same answer the
  // hand-off's `if_match` is taken from. The search hit's assignee is the
  // row as of the SEARCH, which a hand-off since can have moved, so it is
  // only what the dialog says while the read is in flight.
  const holder = task ? (task.assignee ?? "") : (chosen?.assignee ?? "");
  const theirs = !!chosen && holder === handle;
  const blocked = !chosen
    ? "Find the task to hand over first."
    : theirs
      ? `${chosen.key} is already ${name}'s.`
      : version === null
        ? read.error
          ? `${chosen.key} could not be read, so it cannot be handed over safely.`
          : `Reading ${chosen.key}…`
        : undefined;
  const submit = async () => {
    // THE BUTTON'S OWN GATE, because the dialog is a form Enter submits.
    if (!pressable(write, blocked) || !chosen || version === null) return;
    const result = await write.run(
      {
        item: chosen.key,
        assignee: handle,
        if_match: version,
        ...(reason.trim() ? { reason: reason.trim() } : {}),
      },
      { done: `Assigned ${chosen.key} to ${name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="md"
      title={`Assign a task to ${name}`}
      icon={<PlusGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footerStart={
        <Button
          variant="ghost"
          leadingIcon={<PlusGlyph size="sm" />}
          onClick={() => {
            onClose();
            openNewTask({ assignee: handle });
          }}
        >
          New task for {name}
        </Button>
      }
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
            blocked={blocked}
          >
            Assign
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        {/* THE MATCHES ARE IN THE DIALOG'S FLOW (`.assign-seat-find`), not
            the kit's anchored panel: anchored inside a dialog body that
            scrolls, eight matches were clipped to two and a half behind the
            footer, over the field below them. Finding the task is the
            dialog's first step, so its list is the dialog's content while it
            is open, and the dialog grows to hold it. */}
        <FormField label="Find a task" htmlFor="assign-seat-find">
          {(field) => (
            <div className="assign-seat-find">
              <Combobox
                id={field.id}
                // THE LIST'S NAME; the field's is the label above it.
                label="Matching tasks"
                aria-describedby={field.describedBy}
                placeholder="Search by key or words in the task"
                leading={<SearchGlyph size="sm" />}
                value={term}
                onValueChange={(next) => {
                  setTerm(next);
                  setChosen(null);
                  setListOpen(next.trim().length >= ASSIGN_MIN_TERM);
                }}
                open={listOpen && q.length >= ASSIGN_MIN_TERM}
                onOpenChange={setListOpen}
                options={options}
                emptyMessage={
                  search.loading
                    ? "Searching…"
                    : search.data && !search.data.available
                      ? "This node is still indexing the tracker — try again in a moment"
                      : "No task matches"
                }
                onCommit={(option) => {
                  const hit = hits.find((h) => h.key === option.value);
                  if (!hit) return;
                  setChosen({ key: hit.key, title: hit.title, assignee: hit.assignee ?? "" });
                  setTerm(`${hit.key} · ${hit.title}`);
                  setListOpen(false);
                }}
              />
            </div>
          )}
        </FormField>
        {/* THE REASON IS FOR A HAND-OFF, so it is asked once there is one:
            before a task is chosen there is nothing for it to be the reason
            for, and the field only stood under the matches. */}
        {chosen && theirs && (
          // SAID WHERE IT IS READ, not only on a disabled button's title: the
          // match list offers the seat's own tasks too.
          <p className="t-caption">
            {chosen.key} is already {name}&rsquo;s — find another task, or file a new one.
          </p>
        )}
        {chosen && !theirs && (
          <>
            <p className="t-caption">
              {holder
                ? `${chosen.key} is with ${nameOf(holder)} now.`
                : `${chosen.key} is unassigned now.`}
            </p>
            <FormField
              label="Why it is theirs now"
              optional
              htmlFor="assign-seat-reason"
              helper={`${name} is woken with this line, and the task's history shows it beside the hand-off.`}
            >
              <Input
                id="assign-seat-reason"
                value={reason}
                onChange={(event) => setReason(event.target.value)}
              />
            </FormField>
          </>
        )}
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/** The longest reason a pause carries, the engine's own `MaxPauseReasonRunes`. */
const PAUSE_REASON_MAX = 500;

/**
 * Pause a seat, or resume it.
 *
 * A PAUSE HOLDS THE SEAT'S MAIL AND ENDS NOTHING by default: the seat starts
 * no new turn, what is sent to it waits on its inbox in order, its scheduled
 * runs are skipped — and the turn it is on FINISHES FIRST. Stopping that turn
 * is a separate, explicit choice ("Also stop the current turn"), because a
 * turn stopped at its next round loses what it had not done and is not run
 * again; `stop_running` is sent only when it is ticked.
 *
 * RESUME IS ONE PRESS: it changes nothing a person has to weigh, and the mail
 * that waited is delivered first, in order.
 */
export function PauseSeatButton({
  handle,
  name,
  paused,
  working,
  open: held,
  onOpenChange,
}: {
  handle: string;
  name: string;
  /** Whether the seat is paused now (the engine's `paused`). */
  paused: boolean;
  /** Whether it is on a turn now, which the stop choice is about. */
  working: boolean;
  /**
   * Whether the Pause dialog is open, for a page that also opens it from its
   * bar's "More" on a phone — as [EditProjectButton] takes it. Left out, the
   * button holds it.
   */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}) {
  const pause = useAct("pause_seat");
  const resume = useAct("resume_seat");
  const [own, setOwn] = useState(false);
  const open = held ?? own;
  const setOpen = (next: boolean) => {
    setOwn(next);
    onOpenChange?.(next);
  };
  if (paused) {
    return (
      <WriteButton
        write={resume}
        size="small"
        variant="secondary"
        leadingIcon={<RotateCwGlyph size="sm" />}
        onPress={() => void resume.run({ handle }, { done: `Resumed ${name}` })}
      >
        Resume
      </WriteButton>
    );
  }
  return (
    <>
      <WriteButton
        write={pause}
        size="small"
        variant="ghost"
        leadingIcon={<PauseGlyph size="sm" />}
        showRefusal={false}
        onPress={() => setOpen(true)}
      >
        Pause
      </WriteButton>
      {open && (
        <PauseSeatDialog
          handle={handle}
          name={name}
          working={working}
          write={pause}
          onClose={() => {
            pause.dismiss();
            setOpen(false);
          }}
        />
      )}
    </>
  );
}

function PauseSeatDialog({
  handle,
  name,
  working,
  write,
  onClose,
}: {
  handle: string;
  name: string;
  working: boolean;
  write: ReturnType<typeof useAct<"pause_seat">>;
  onClose: () => void;
}) {
  const [reason, setReason] = useState("");
  const [stop, setStop] = useState(false);
  const long = [...reason.trim()].length > PAUSE_REASON_MAX;
  const blocked = long
    ? `A reason is one line — at most ${PAUSE_REASON_MAX} characters.`
    : undefined;
  const submit = async () => {
    // THE BUTTON'S OWN GATE, for the dialog's submit.
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      {
        handle,
        ...(reason.trim() ? { reason: reason.trim() } : {}),
        // SENT ONLY WHEN CHOSEN: absent is the engine's default, the turn
        // finishing first.
        ...(stop ? { stop_running: true } : {}),
      },
      { done: stop ? `Paused ${name} and stopped its turn` : `Paused ${name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="md"
      title={`Pause ${name}`}
      icon={<PauseGlyph />}
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
            blocked={blocked}
          >
            Pause
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <p className="t-body">
          {name} starts no new turn until somebody resumes it. Work sent to it waits on its inbox,
          in order, and its scheduled runs are skipped rather than queued.
        </p>
        <FormField
          label="Why"
          optional
          htmlFor="pause-seat-reason"
          helper="One line, shown on the seat and in the feed."
        >
          <Input
            id="pause-seat-reason"
            value={reason}
            onChange={(event) => setReason(event.target.value)}
          />
        </FormField>
        <Checkbox
          checked={stop}
          onChange={(event) => setStop(event.target.checked)}
          label="Also stop the current turn"
          description={
            working
              ? "Without it, the turn it is on finishes first. Stopped, it ends at its next round, what it had not done is lost, and it is not run again."
              : "It is not on a turn now. Without it, a turn that starts before the pause lands finishes first."
          }
        />
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// A running turn's own control: a note it reads at its next round
// ---------------------------------------------------------------------------

/**
 * Why a turn cannot take a note now, or undefined when it can.
 *
 * A NOTE REACHES A TURN AT THE TOP OF ITS NEXT ROUND IN THE ENGINE'S OWN LOOP
 * (`internal/agent/steer`), so it is offered only while that loop is running.
 * A turn parked on a coding run is inside somebody else's loop — a run it
 * launched, or an executor that runs as a coding agent — which reads no notes,
 * and the engine would answer `steer_unsupported` or `not_running` one press
 * too late. Each reason names what to do instead.
 */
function steerBlocked(state: { running: boolean; parked: boolean }): string | undefined {
  if (state.parked) {
    return (
      "This turn is waiting inside a coding agent's own loop, which reads no notes. " +
      "Answer the run, or write on the task."
    );
  }
  if (!state.running) return "The turn has ended — a note reaches only a turn that is running.";
  return undefined;
}

/**
 * Steer a running turn: a short note the turn reads at its next round, after
 * the tool call in flight returns, and keeps to for the rest of the turn.
 *
 * WHAT A PRESS PROMISES IS `pending`: the node running the turn took the note.
 * What became of it — read at a round, or expired because the turn ended first
 * — is the turn's own `agent_turn_steered`, which the trace draws at the round
 * that read it. A retry of an unknown answer reuses the request id, and the
 * request id IS the note's id, so the turn takes it once.
 */
export function SteerTurnButton({
  turnId,
  seat,
  running,
  parked,
}: {
  turnId: string;
  /** Whose turn it is, by name, for the dialog's title and the toast. */
  seat: string;
  running: boolean;
  parked: boolean;
}) {
  const write = useAct("steer_turn");
  const [open, setOpen] = useState(false);
  return (
    <>
      <WriteButton
        write={write}
        size="small"
        variant="secondary"
        leadingIcon={<CompassGlyph />}
        showRefusal={false}
        blocked={steerBlocked({ running, parked })}
        onPress={() => setOpen(true)}
      >
        Steer
      </WriteButton>
      {open && (
        <SteerTurnDialog
          turnId={turnId}
          seat={seat}
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

function SteerTurnDialog({
  turnId,
  seat,
  write,
  onClose,
}: {
  turnId: string;
  seat: string;
  write: ReturnType<typeof useAct<"steer_turn">>;
  onClose: () => void;
}) {
  const [note, setNote] = useState("");
  const length = [...note.trim()].length;
  const blocked =
    length === 0
      ? "Write the note first."
      : length > STEER_NOTE_MAX_RUNES
        ? `A note is at most ${STEER_NOTE_MAX_RUNES} characters — write anything longer on the task.`
        : undefined;
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { turn_id: turnId, note: note.trim() },
      { done: `Sent your note to ${seat}'s turn` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="md"
      title={`Steer ${seat}'s turn`}
      icon={<CompassGlyph />}
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
            blocked={blocked}
          >
            Send note
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Note"
          htmlFor="steer-turn-note"
          helper={`Read at the turn's next round, after the call in flight returns, and kept to for the rest of the turn. ${length} of ${STEER_NOTE_MAX_RUNES} characters.`}
        >
          <Textarea
            id="steer-turn-note"
            rows={4}
            value={note}
            onChange={(event) => setNote(event.target.value)}
          />
        </FormField>
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}
