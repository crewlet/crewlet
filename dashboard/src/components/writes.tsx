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

import { usePresentedDialog } from "./dialogPresenter.tsx";
import { useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Checkbox,
  ChoiceList,
  Combobox,
  Composer,
  FormField,
  Input,
  Modal,
  Prose,
  ReadingPaneReasoning,
  Select,
  Stack,
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
  SearchGlyph,
  CompassGlyph,
} from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton, pressable } from "./WriteButton.tsx";
import { QueryState } from "./common.tsx";
import { useAct } from "~/lib/useAct.ts";
import { isAccessOption } from "~/lib/grants.ts";
import { AccessGrantDialog } from "~/components/advisorFix.tsx";
import { AnswerChoices } from "./AnswerChoices.tsx";
import { Recommendation } from "./Recommendation.tsx";
import { useAnswerHold, useMessageSeatAccess } from "~/lib/useWriteAccess.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { handleLabel, indexOrg, nameOfIn } from "~/lib/seats.ts";
import { itemAddress, targetLabel } from "~/lib/work.ts";
import { useOpenNewTask } from "~/app/newTask.ts";
import { STEER_NOTE_MAX_RUNES } from "~/contract/steer.ts";
import { steerReadsSentence } from "~/lib/steer.ts";
import { VIEW_NAME_MAX_BYTES } from "~/contract/work.ts";
import { possessive, textBudget } from "~/lib/format.ts";
import { CHECKED_WORDS, renderMarkdown, renderTokens } from "~/lib/markdown.ts";
import { RUNS_POLL_MS, runAskTexts, unanswerable } from "~/lib/runs.ts";
import { RunQuestion } from "./RunQuestion.tsx";
import { HeldCallButtons } from "./HeldCallButtons.tsx";
import { TokenScope } from "./InlineToken.tsx";
import type { ExecutorRow } from "~/protocol/index.ts";
import { useServicesRun } from "~/lib/services.ts";

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
  label,
  version,
  assignee,
}: {
  /**
   * The task's address ([itemAddress]): its key (ENG-42), or its id where
   * another task claimed that key first and the key would hand off the
   * claimant.
   */
  item: string;
  /** What the dialog's words call the task — its key — where `item` is its id. */
  label?: string;
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
          named={label || item}
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
  named,
  version,
  assignee,
  write,
  onClose,
}: {
  item: string;
  /** What the dialog's words call the task. */
  named: string;
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
  // NOBODY HOLDS IT YET: the dialog opens on "Nobody", which is no change, so it
  // says what the press needs rather than naming a holder there is not.
  const blocked =
    who !== assignee
      ? undefined
      : assignee
        ? "Choose somebody other than who holds it now."
        : "Choose who to assign it to.";
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
      { done: who === NOBODY ? `Unassigned ${named}` : `Assigned ${named} to ${name}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Assign ${named}`}
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
            {who === NOBODY && assignee !== NOBODY ? "Unassign" : "Assign"}
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
            helper="Sent to the new assignee and kept in history."
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
export function RestoreButton({
  item,
  label,
}: {
  /**
   * The task's address ([itemAddress]): restored by its key, a duplicate in
   * the trash beside the task that claimed its key would bring back the
   * claimant — or be refused because the claimant was never removed.
   */
  item: string;
  /** What the control and its toast call the task — its key — where `item` is its id. */
  label?: string;
}) {
  const write = useAct("restore_work_item");
  const named = label || item;
  return (
    <WriteButton
      write={write}
      size="small"
      variant="ghost"
      leadingIcon={<UndoGlyph />}
      aria-label={`Restore ${named}`}
      onPress={() => void write.run({ item }, { done: `Restored ${named}` })}
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
        done: day
          ? `Set ${possessive(name)} target to ${targetLabel(day)}`
          : `Cleared ${possessive(name)} target date`,
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
          helper="Company time zone. Leave empty to clear."
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
 * (`personal`): a shared view is in everybody's strip, and saving somebody's
 * private query into it is a change to their screen too. WHOSE it is then is
 * the engine's answer and never an argument — the caller's own record, the
 * one their pins are kept under — so the dialog sends a flag rather than a
 * name somebody could fill with another person's.
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
  const budget = textBudget(name, VIEW_NAME_MAX_BYTES, "a view's name");
  const blocked = !name.trim()
    ? "Give the view a name first."
    : budget.over
      ? `Shorten the name: it is ${budget.overBy}.`
      : undefined;
  const submit = async () => {
    // THE BUTTON'S OWN GATE, because the dialog is a form its one field
    // submits on Enter: a second Enter saved a second view.
    if (!pressable(write, blocked)) return;
    const title = name.trim();
    const result = await write.run(
      { container, name: title, type, params, ...(mine ? { personal: true } : {}) },
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
          helper={
            budget.over
              ? undefined
              : (budget.line ?? "The tab's label, in the strip above the work.")
          }
          error={budget.over ? budget.line : undefined}
        >
          <Input
            id="save-view-name"
            value={name}
            error={budget.over}
            onChange={(event) => setName(event.target.value)}
            autoFocus
          />
        </FormField>
        <Checkbox
          checked={mine}
          onChange={(event) => setMine(event.target.checked)}
          label="Only for me"
          description="Otherwise everyone sees it."
        />
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * How an ask was answered, for the line that replaces its controls.
 *
 * `recommended` is set on a choice that was the asker's own recommendation,
 * which the line says beside it, and `id` names the option, which the options
 * drawn under an answer mark as chosen.
 */
export type AskAnswer =
  | { kind: "choice"; id: string; label: string; recommended: boolean }
  | { kind: "reply"; text: string };

/** The value the "Something else" choice carries in the list of options. */
const OTHER = "\u0000other";

/**
 * The longest reply a decision's "Something else" takes. A reply here is one
 * direction for the asker, read into its next turn; the engine's comment
 * ceiling (32 KiB) is the bound on a whole comment and far past what a reader
 * types into a card, so the field counts down from here instead.
 */
const DECISION_REPLY_MAX = 2000;

type AskOption = { id: string; label: string; detail?: string };

/** What a grant option's button says before the engine's words arrive. */
const GRANT_LABEL = "Review the access change";

/**
 * A grant option drawn in the engine's words (ADR-0118), never the asker's:
 * the label and note of the change `advisor_access` builds for the asker.
 */
function useGrantWords(options: readonly AskOption[], seat: string): readonly AskOption[] {
  const grant = options.find((o) => isAccessOption(o.id))?.id;
  const { data } = useQuery("advisor_access", { seat, access: grant }, { enabled: !!grant });
  const words = data?.fix.options[0];
  return useMemo(
    () =>
      options.map((o) =>
        isAccessOption(o.id)
          ? {
              id: o.id,
              label: o.id === grant && words ? words.label : GRANT_LABEL,
              detail: o.id === grant && data ? data.fix.unchanged : undefined,
            }
          : o,
      ),
    [options, grant, words, data],
  );
}

/** What an ask is answered from; see [AnswerAsk]. */
type AnswerAskProps = {
  /**
   * The task's address ([itemAddress]): its key, or its id where another
   * task claimed that key first and the key would answer on the claimant.
   */
  item: string;
  /** What the answer's toast calls the task (its key) where `item` is its id. */
  label?: string;
  /** The ask's comment id, which the answer names. */
  comment: string;
  /** Who asked, by name, for the reply field's placeholder. */
  asker: string;
  /** The ask's options; empty for an ask answered in words. */
  options: readonly { id: string; label: string; detail?: string }[];
  /** The option the asker recommends, if any. */
  recommended?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called once the engine has the answer. */
  onAnswered: (answer: AskAnswer) => void;
  /** How the answer is laid out; see above. */
  layout?: "line" | "row" | "buttons";
  /** The asker's handle: whose access a grant option would change. */
  askedBy: string;
  /** The question asked, for a grant's preview title. */
  question?: string;
};

/**
 * Answer an ask in place: accept its recommendation, choose another option,
 * or reply in your own words.
 *
 * AT REST, ONE LINE: "Recommended: Greedy only" with Accept and Change.
 * Accept sends the recommended option; Change opens every option with its
 * detail line, plus "Something else", whose field sends a reply instead of a
 * choice. An ask with no recommendation offers Answer in their place, and an
 * ask with no options at all offers Reply, which opens the field alone. While
 * the options are open the recommendation line is hidden: the list says it
 * on the option itself.
 *
 * THE CHOICE IS AN ANSWER TO THE ASK IT BELONGS TO (`answers` names the ask's
 * comment), so the engine checks the option is one the ask offered, closes the
 * ask, and wakes the asker with what was chosen. A reply is the same answer
 * with a body instead of a choice. ONE WRITE FOR THE WHOLE ASK: an ask takes
 * one answer, so every way of sending one goes through the one `useAct`, and
 * nothing else is pressable while it is in flight.
 *
 * THE OPEN STATE IS THE CALLER'S, so a card that lists several asks keeps one
 * open at a time: opening one closes whichever was.
 *
 * THREE LAYOUTS OF ONE ANSWER. `line` is the one above, for a thread where
 * an ask is one entry among others. `row` is for a list of decisions, where
 * the row already says the recommendation beside the ask's key
 * ([Recommendation]): at rest it draws only Accept and Change, Change as a
 * quiet button so Accept is the one filled control on the row, and open it is
 * `line`'s open state. `buttons` is for a screen where the question is the
 * whole of what is open (the Inbox's pane): every option is a button that
 * sends it with one press (`AnswerChoices`, which an executor's options are
 * drawn with too), the recommended one first and filled, and
 * "Something else" opens the field in their place. An ask with no options is
 * answered in that field from the start. Whichever the layout, the answer
 * goes through this one write, so the rules above hold for all three.
 */
export function AnswerAsk(props: AnswerAskProps) {
  // A GRANT IS DRAWN IN THE ENGINE'S WORDS, read only where an option is one.
  return props.options.some((o) => isAccessOption(o.id)) ? (
    <GrantAnswer {...props} />
  ) : (
    <AskAnswer {...props} />
  );
}

function GrantAnswer(props: AnswerAskProps) {
  return <AskAnswer {...props} options={useGrantWords(props.options, props.askedBy)} />;
}

function AskAnswer({
  item,
  comment,
  asker,
  options,
  recommended,
  open,
  onOpenChange,
  onAnswered,
  layout = "line",
  label,
  askedBy,
  question,
}: AnswerAskProps) {
  const write = useAct("comment_on_work_item");
  const named = label || item;
  const rec = options.find((o) => o.id === recommended);
  const words = options.length === 0;
  const [chosen, setChosen] = useState<string>(rec?.id ?? (words ? OTHER : ""));
  const [text, setText] = useState("");

  const close = () => {
    write.dismiss();
    setChosen(rec?.id ?? (words ? OTHER : ""));
    onOpenChange(false);
  };

  // A GRANT IS PREVIEWED AND SAVED BEFORE IT IS ANSWERED (ADR-0118), so a
  // person who chose it never leaves the agent told yes with nothing changed.
  const [granting, setGranting] = useState<string | null>(null);
  const send = async (choice: string) => {
    if (isAccessOption(choice) && options.some((o) => o.id === choice)) {
      setGranting(choice);
      return;
    }
    await answer(choice);
  };

  const answer = async (choice: string) => {
    const option = options.find((o) => o.id === choice);
    const body = text.trim();
    const result = option
      ? await write.run(
          { item, answers: comment, choice: option.id },
          { done: `Answered ${named}: ${option.label}` },
        )
      : await write.run(
          { item, answers: comment, body },
          { done: `Replied to ${asker} on ${named}` },
        );
    if (!result || (result.kind !== "applied" && result.kind !== "pending")) return;
    onAnswered(
      option
        ? {
            kind: "choice",
            id: option.id,
            label: option.label,
            recommended: option.id === recommended,
          }
        : { kind: "reply", text: body },
    );
  };

  const other = chosen === OTHER;
  const blocked = other
    ? text.trim()
      ? undefined
      : "Write the reply first."
    : chosen
      ? undefined
      : "Choose an answer first.";
  const submit = () => {
    if (pressable(write, blocked)) void send(chosen);
  };
  // ONE FIELD'S WORDS FOR EVERY LAYOUT: "instead" only beside options it
  // replaces, and one verb for sending an answer.
  const placeholder = words ? `Tell ${asker} what you want` : `Tell ${asker} what you want instead`;
  const actions = (
    <>
      <Button size="small" variant="ghost" onClick={close} disabled={write.busy}>
        Cancel
      </Button>
      <WriteButton
        write={write}
        size="small"
        variant="primary"
        blocked={blocked}
        showRefusal={false}
        onPress={submit}
      >
        Send
      </WriteButton>
    </>
  );
  const composer = (
    <Composer
      label={words ? `Reply to ${asker}` : "Something else"}
      value={text}
      onChange={setText}
      onSubmit={submit}
      maxLength={DECISION_REPLY_MAX}
      placeholder={placeholder}
      disabled={!write.access.can || write.busy}
      actions={actions}
      autoFocus
    />
  );

  const view = (): ReactNode => {
    if (layout === "buttons") {
      // THE FIELD IN THE OPTIONS' PLACE, once "Something else" opened it, and
      // from the start for an ask that offers none. It takes the focus only
      // when a press opened it: stepping onto an ask with j or k must not drop
      // the reader into a field.
      if (words || open) {
        return (
          <Stack gap={2}>
            <Composer
              label={`Reply to ${asker}`}
              value={text}
              onChange={setText}
              onSubmit={submit}
              maxLength={DECISION_REPLY_MAX}
              placeholder={placeholder}
              disabled={!write.access.can || write.busy}
              autoFocus={!words}
              actions={
                <>
                  {!words && (
                    <Button size="small" variant="ghost" onClick={close} disabled={write.busy}>
                      Cancel
                    </Button>
                  )}
                  <WriteButton
                    write={write}
                    size="small"
                    variant="primary"
                    blocked={text.trim() ? undefined : "Write the reply first."}
                    showRefusal={false}
                    onPress={submit}
                  >
                    Send
                  </WriteButton>
                </>
              }
            />
            <RefusalNote write={write} />
          </Stack>
        );
      }
      return (
        <AnswerChoices
          write={write}
          options={options}
          recommended={recommended}
          pending={write.busy ? chosen : null}
          onChoose={(choice) => {
            setChosen(choice);
            void send(choice);
          }}
          onSomethingElse={() => {
            setChosen(OTHER);
            onOpenChange(true);
          }}
        />
      );
    }

    if (!open) {
      const row = layout === "row";
      return (
        <div className={row ? "decision-answer decision-answer--row" : "decision-answer"}>
          {!row && <Recommendation options={options} recommended={recommended} />}
          <span className="decision-answer-actions">
            {rec && (
              <WriteButton
                write={write}
                size="small"
                variant="primary"
                showRefusal={false}
                onPress={() => void send(rec.id)}
              >
                Accept
              </WriteButton>
            )}
            <WriteButton
              write={write}
              size="small"
              variant={rec ? (row ? "ghost" : "secondary") : "primary"}
              showRefusal={false}
              pressing={false}
              blocked={write.busy ? "Your answer to this ask is being sent" : undefined}
              onPress={() => onOpenChange(true)}
            >
              {rec ? "Change" : words ? "Reply" : "Answer"}
            </WriteButton>
          </span>
          <RefusalNote write={write} />
        </div>
      );
    }

    return (
      <div className="decision-answer is-open">
        {words ? (
          composer
        ) : (
          <ChoiceList
            label="Answers"
            value={chosen}
            onChange={setChosen}
            disabled={!write.access.can || write.busy}
            options={[
              ...options.map((o) => ({
                value: o.id,
                label: o.label,
                detail: o.detail,
                tag: o.id === recommended ? "Recommended" : undefined,
              })),
              {
                value: OTHER,
                label: "Something else",
                detail: "Reply in your own words instead.",
                separated: true,
                panel: composer,
              },
            ]}
            footer={other ? undefined : actions}
          />
        )}
        <RefusalNote write={write} />
      </div>
    );
  };

  const grant = granting && (
    <AccessGrantDialog
      seat={askedBy}
      access={granting}
      question={question ?? "Allow this access?"}
      onClose={() => setGranting(null)}
      onApplied={() => {
        setGranting(null);
        void answer(granting);
      }}
    />
  );
  return (
    <>
      {view()}
      {grant}
    </>
  );
}

/**
 * Answer an executor that stopped to ask a person something, by its turn,
 * which reaches it whatever woke the turn that launched it, chat thread or
 * none.
 *
 * `pending` IS THE ORDINARY ANSWER: the answer is on the seat's inbox and the
 * node holding the seat launches the executor again with it, which the
 * executor's own record says once it has.
 */
export function AnswerRunButton({
  run,
  seat,
}: {
  /** The parked executor: its turn and the question it asks, which the answer names. */
  run: AnsweredRun;
  /** Whose executor it is, by name, for the dialog's title. */
  seat: string;
}) {
  // WHO MAY ANSWER IS THE SEAT'S RULE, for the question and a held call alike
  // (`useAnswerHold`): held with who may, on hover and on keyboard focus.
  const held = useAnswerHold(run.agent_handle);
  // DRAWN WHERE THE SCREEN PRESENTS IT, outside a table's layer host (`dialogPresenter.tsx`).
  const [dialog, show] = usePresentedDialog();
  // A RUN HOLDING A CALL FOR APPROVAL is answered through the approvals
  // route: its own answer is refused for a held call.
  if (run.held_call)
    return (
      <HeldCallButtons
        id={run.held_call.id}
        seat={run.agent_handle}
        fixed={run.held_call.fixed}
        approvers={run.held_call.approvers}
        act={run.held_call.act}
      />
    );
  return (
    <>
      <Button
        tooltip
        size="small"
        variant="secondary"
        leadingIcon={<CornerDownLeftGlyph />}
        disabledReason={held}
        onClick={(event) => {
          // A ROW OR A CARD IS OFTEN AN ANCHOR: the press is this button's.
          event.preventDefault();
          event.stopPropagation();
          show(<AnswerRunDialog run={run} seat={seat} onClose={() => show(null)} />);
        }}
      >
        Answer
      </Button>
      {dialog}
    </>
  );
}

/**
 * What the answer dialog is handed of a parked executor: a durable row, or a
 * live entry, which carries no launch.
 */
type AnsweredRun = Pick<
  ExecutorRow,
  | "agent_handle"
  | "turn_id"
  | "launch_id"
  | "question"
  | "question_context"
  | "decision"
  | "held_call"
>;

/**
 * The executor's question as the Inbox reads it: its title (`RunQuestion`),
 * its context as Markdown, the executor's recommendation, and, for a question
 * that asked to choose, its options as the buttons an ask's are
 * (`AnswerChoices`), each answering with one press. "Something else", and an
 * open question from the start, answers in words.
 *
 * READ OFF THE EXECUTOR'S DURABLE ROW where the dialog has it, because THE
 * ANSWER NAMES THE QUESTION DRAWN (`launch_id`), and the live projection a
 * seat's page draws from carries no launch: until the row is read, nothing is
 * sent ([unanswerable]). An executor that has moved on to another question
 * refuses the answer `conflict`, in the engine's words, and the refusal asks
 * the durable row again, so the dialog then draws the question it asks now
 * under that sentence.
 */
function AnswerRunDialog({
  run,
  seat,
  onClose: close,
}: {
  run: AnsweredRun;
  seat: string;
  onClose: () => void;
}) {
  // ITS OWN WRITE: a presented dialog is drawn away from the button that opened it.
  const write = useAct("answer_executor");
  const onClose = () => {
    write.dismiss();
    close();
  };
  const runs = useQuery("executors", undefined, { pollMs: RUNS_POLL_MS });
  const shown = runs.data?.runs.find((r) => r.turn_id === run.turn_id) ?? run;
  const question = (shown.question ?? "").trim();
  const context = shown.question_context.trim();
  const decision = shown.decision;
  const options = decision?.options ?? [];
  const recommended = options.find((o) => o.id === decision?.recommended);
  // "SOMETHING ELSE" OPENED THE FIELD in the options' place; a question with
  // no options is answered in it from the start.
  const [open, setOpen] = useState(false);
  const inWords = options.length === 0 || open;
  const [chosen, setChosen] = useState<string | null>(null);
  const [answer, setAnswer] = useState("");
  const notYet = unanswerable(shown);
  const blocked = notYet ?? (answer.trim() ? undefined : "Write the answer first.");
  const send = async (args: { choice: string } | { answer: string }) => {
    const result = await write.run(
      { turn_id: run.turn_id, launch_id: shown.launch_id, ...args },
      { done: `Sent your answer to ${possessive(seat)} executor` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  const submit = () => {
    // THE BUTTON'S OWN GATE, for the dialog's submit.
    if (!pressable(write, blocked)) return;
    void send({ answer: answer.trim() });
  };
  return (
    <Modal
      open
      size="md"
      title={`Answer ${possessive(seat)} executor`}
      icon={<CornerDownLeftGlyph />}
      onClose={onClose}
      onSubmit={inWords ? submit : undefined}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          {inWords && (
            <WriteButton
              write={write}
              variant="primary"
              showRefusal={false}
              onPress={submit}
              blocked={blocked}
            >
              Send answer
            </WriteButton>
          )}
        </>
      }
    >
      <TokenScope texts={runAskTexts(shown)}>
        <div className="col gap-3">
          {question && <RunQuestion question={question} as="dialog" />}
          {context && (
            <Prose as="div" measure="none" className="prose md">
              {renderMarkdown(context, { ...CHECKED_WORDS, scoped: true })}
            </Prose>
          )}
          {recommended && decision?.rationale && (
            <ReadingPaneReasoning
              title={
                <>
                  {`${seat} recommends `}
                  <strong>{renderTokens(recommended.label, "recommended", "text", true)}</strong>
                </>
              }
            >
              <p>{renderTokens(decision.rationale, "reasoning", "checked", true)}</p>
            </ReadingPaneReasoning>
          )}
          {inWords ? (
            <>
              <FormField
                label="Your answer"
                htmlFor="answer-run-text"
                helper="The executor carries on with your answer."
              >
                <Textarea
                  id="answer-run-text"
                  rows={5}
                  value={answer}
                  autoFocus={open}
                  onChange={(event) => setAnswer(event.target.value)}
                />
              </FormField>
              <RefusalNote write={write} />
            </>
          ) : (
            <AnswerChoices
              write={write}
              options={options}
              recommended={decision?.recommended}
              pending={write.busy ? chosen : null}
              blocked={notYet}
              onChoose={(choice) => {
                setChosen(choice);
                void send({ choice });
              }}
              onSomethingElse={() => setOpen(true)}
            />
          )}
        </div>
      </TokenScope>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// A seat's own controls: message it, give it work, pause it
// ---------------------------------------------------------------------------

/**
 * Message a seat — as a task that ASKS it.
 *
 * MESSAGE IS THE TRACKER'S ASK, and it stays that wherever chat is. It opens
 * the one New task sheet with the seat as assignee and `ask` naming it, so the
 * question is filed as work the seat owes an answer on, and the answer lands
 * in the asker's Inbox. A company on the engine's own chat also offers a
 * DIRECT conversation with the seat beside it (`routes/agents/DirectMessage.
 * tsx`) — a remark that obliges nothing — and the two are different gestures,
 * so neither stands in for the other. The sheet's Create is the write; this
 * button opens it, and is held with the reason where the reader could not
 * file one — a door that opened onto a sheet that then refused would teach
 * the reason one press too late.
 *
 * AND HELD ON THE READER'S OWN SEAT, by itself (`useMessageSeatAccess`): an
 * ask of your own seat is a question only you could answer. Decided here
 * rather than by each screen that draws the button, because the page held it
 * while the peek drew it bare.
 */
export function MessageSeatButton({
  handle,
  variant = "secondary",
}: {
  handle: string;
  variant?: "primary" | "secondary";
}) {
  const write = useAct("create_work_item");
  const access = useMessageSeatAccess(handle);
  const openNewTask = useOpenNewTask();
  // A MESSAGE IS AN ASK FILED AS A TASK, so where the company runs no
  // Worklet there is nothing to file it in and no button (`lib/services.ts`).
  const runs = useServicesRun();
  if (!runs.tracker) return null;
  return (
    <WriteButton
      write={write}
      size="small"
      variant={variant}
      leadingIcon={<MessageSquareGlyph size="sm" />}
      showRefusal={false}
      // THE ACT'S OWN REASON IS THE BUTTON'S where it cannot act at all, and
      // the access above is that same reason then — so this is only ever the
      // own-seat hold, which ranks after everything else.
      blocked={access.can ? undefined : access.reason}
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
  hits: readonly {
    id: string;
    key: string;
    key_collision?: boolean;
    title: string;
    assignee?: string;
  }[],
  handle: string,
  nameOf: (handle: string) => string,
): ComboboxOption[] {
  const movable = hits.filter((h) => (h.assignee ?? "") !== handle);
  const theirs = hits.filter((h) => (h.assignee ?? "") === handle);
  return [...movable, ...theirs].slice(0, ASSIGN_MATCHES).map((hit) => {
    const own = (hit.assignee ?? "") === handle;
    return {
      // THE ID IS THE VALUE, the key the label: a key two tasks hold is one
      // value for two options, and the one that did not claim it is reached
      // by its id alone (`itemAddress`).
      value: hit.id,
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
  // A TASK TO ASSIGN IS WORKLET'S, so where the company runs none there
  // is no button (`lib/services.ts`).
  const runs = useServicesRun();
  if (!runs.tracker) return null;
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
  const [chosen, setChosen] = useState<{
    id: string;
    /** What the engine opens this task by ([itemAddress]). */
    address: string;
    key: string;
    title: string;
    assignee: string;
  } | null>(null);
  const [reason, setReason] = useState("");
  const q = term.trim();
  // THE BOUND IS THE HOOK'S: a pasted description past it is not sent, and
  // the list says why rather than "No task matches" (`useSearchQuery`).
  const search = useSearchQuery(
    "work_search",
    { q, mode: "hybrid", limit: ASSIGN_SEARCH },
    { enabled: q.length >= ASSIGN_MIN_TERM },
  );
  // THE VERSION THE HAND-OFF IS CONDITIONAL ON, read for the task chosen.
  const read = useQuery("work_item", chosen ? { id: chosen.address } : undefined, {
    enabled: chosen !== null,
  });
  // A REFUSED SEARCH LISTS NOTHING: `useQuery` keeps its last answer across
  // a failure, which here is the hits of a term the box no longer holds.
  const hits = q.length >= ASSIGN_MIN_TERM && !search.error ? (search.data?.hits ?? []) : [];
  const options = useMemo(() => assignOptions(hits, handle, nameOf), [hits, handle, nameOf]);
  const task = read.data && read.data.task.id === chosen?.id ? read.data.task : null;
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
      ? `${chosen.key} is already ${possessive(name)}.`
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
        item: chosen.address,
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
                  search.tooLong ??
                  search.withheld ??
                  (search.error ? (
                    <QueryState
                      error={search.error}
                      refusal={search.refusal}
                      detail={search.detail}
                      loading={false}
                    />
                  ) : search.loading ? (
                    "Searching…"
                  ) : search.data && !search.data.available ? (
                    "Still indexing. Try again shortly."
                  ) : (
                    "No task matches"
                  ))
                }
                onCommit={(option) => {
                  const hit = hits.find((h) => h.id === option.value);
                  if (!hit) return;
                  setChosen({
                    id: hit.id,
                    address: itemAddress(hit),
                    key: hit.key,
                    title: hit.title,
                    assignee: hit.assignee ?? "",
                  });
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
            {chosen.key} is already assigned to {name}.
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
              helper={`Sent to ${name} and kept in history.`}
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

// ---------------------------------------------------------------------------
// A running turn's own control: a note it reads at its next step
// ---------------------------------------------------------------------------

/**
 * Why a turn cannot take a note now, or undefined when it can.
 *
 * EVERY TURN THAT IS STILL GOING TAKES ONE (`internal/agent/steer`): a turn in
 * the engine's own loop reads it at its next round, one whose executor is in
 * its box keeps it on the executor's record — read by the executor at its next
 * step, or after its question is answered, or by the review that follows it.
 * Only a turn that has ended is held, because nothing would ever read the note.
 */
function steerBlocked(running: boolean): string | undefined {
  return running ? undefined : "The turn has ended.";
}

/**
 * Steer a running turn: a short note the turn reads at its next step — a round
 * of the engine's own loop, or a step of its executor in its box — and keeps
 * to for the rest of the turn.
 *
 * WHAT A PRESS PROMISES IS `pending`: the engine took the note, and says when
 * the turn will read it (`reads`), which the toast says. What became of it —
 * read at a round or by the executor, or expired because the turn ended first — is
 * the turn's own `agent_turn_steered`, which the trace draws where it was read.
 * A retry of an unknown answer reuses the request id, and the request id IS
 * the note's id, so the turn takes it once.
 */
export function SteerTurnButton({
  turnId,
  seat,
  running,
}: {
  turnId: string;
  /** Whose turn it is, by name, for the dialog's title and the toast. */
  seat: string;
  running: boolean;
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
        blocked={steerBlocked(running)}
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

export function SteerTurnDialog({
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
        ? `Notes are limited to ${STEER_NOTE_MAX_RUNES} characters.`
        : undefined;
  const submit = async () => {
    if (!pressable(write, blocked)) return;
    const result = await write.run(
      { turn_id: turnId, note: note.trim() },
      { done: `Sent your note to ${possessive(seat)} turn`, landed: steerReadsSentence },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="md"
      title={`Steer ${possessive(seat)} turn`}
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
          helper={`The turn reads it at its next step. ${length} of ${STEER_NOTE_MAX_RUNES} characters.`}
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
