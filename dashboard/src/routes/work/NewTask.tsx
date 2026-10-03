/**
 * Filing a task, as the signed-in person — the one sheet every "New task"
 * opens.
 *
 * # One record, everything a task starts with
 *
 * The sheet sends ONE `create_work_item` carrying the whole task: its project,
 * title and description, its type, the status it starts in, who holds it, its
 * priority, when it is due and its labels. One record because the engine's
 * create files all of that together — a sheet that filed a title and then
 * patched the rest would be a second write free to fail after the first had
 * put a half-described task on everybody's board.
 *
 * # The project decides the vocabulary
 *
 * A task's type, its statuses' names and the labels it may carry are the
 * PROJECT's (`work_project`), so the three fields are read from the project
 * chosen and move with it: a label from one project is refused on another,
 * and offering it would be offering the refusal. The status list is the
 * project's own words for the six the engine ships.
 *
 * # Who holds it is a seat the chart knows, and the engine says who you mean
 *
 * The assignee field completes against the org chart by name and handle, and
 * asks the engine's own resolver (`colleague`) what the typed words mean —
 * the same four tiers a seat's `lookup_colleague` uses. The engine's match is
 * offered FIRST, as the "best match", and why it matched ("part of the name
 * matches") is said once that seat is taken; several candidates are a list
 * and never a guess. Left empty the task lands where the project sends
 * unassigned work — its default assignee, else triage for its lead — and the
 * field says which before anybody presses anything.
 *
 * WORDS ARE NOT A SEAT. What is typed is not an assignee until a seat is taken
 * from the list, and words left in the field unchosen HOLD Create rather than
 * being dropped: the field used to keep them on screen while the create went
 * out with no assignee at all, so a person who typed "Agent SWE" and pressed
 * Create filed the task to the project's routing with the name still showing
 * above a line that began "Left empty".
 *
 * # What would be refused is said on the field, before the press
 *
 * The engine refuses a title or a description past its cap rather than
 * cutting it (`tracker.MaxTitle`, `MaxBody`, held here by the contract), and a
 * refusal that comes back after the press lands on no field: a person who
 * typed a 600-byte title was answered in a line under the form with the Title
 * field unmarked. So the sheet counts BYTES as the engine does, marks the
 * field that is over (`aria-invalid`, the kit's error line saying by how
 * much), counts down as it nears the cap, and holds Create with the reason
 * written in the footer — visibly, because a disabled button's reason read
 * only by a screen reader is a dead button to everybody else. Pressing Enter
 * while held takes focus to the field that is holding it.
 *
 * # Applied opens it; anything else says so
 *
 * An `applied` answer carries the new key, and the sheet opens the task — the
 * reader filed it to work on it. `pending` closes the sheet with the frame's
 * "not applied yet" toast rather than opening a page this node cannot draw
 * yet. A refusal stays in the sheet beside the fields, in the engine's own
 * sentence, which names the argument it refused ("`labels` …", a required
 * field by its slug) — the one place a person can fix it.
 */

import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import {
  Button,
  Combobox,
  FormField,
  Input,
  Modal,
  Select,
  TagsInput,
  Textarea,
  type ComboboxOption,
  type SelectOption,
} from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import type { NewTaskPreset } from "~/app/newTask.ts";
import { colleagueSendable } from "~/app/palette/hits.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { handleLabel, indexOrg, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { projectsByUnit } from "~/lib/orgchart.ts";
import { DEFAULT_TYPE, PRIORITIES, STATUSES, statusLabel, typeName } from "~/lib/work.ts";
import { humanize, utf8Bytes } from "~/lib/format.ts";
import { TASK_BODY_MAX_BYTES, TASK_TITLE_MAX_BYTES } from "~/contract/work.ts";
import type { WorkProjectDetail, WorkProjectRow } from "~/protocol/index.ts";

/** How many seats the assignee field offers at once. */
const SEAT_CHOICES = 8;

/**
 * The most of a reader's typed words a hold quotes back to them — about a
 * name and a half, which identifies the words at the sheet's foot without
 * letting a pasted paragraph become the sentence.
 */
const QUOTE_CHARS = 40;

/**
 * How near its cap a text is before its field starts counting — the last
 * fifth. Earlier than that a counter is a number a reader is not deciding
 * anything with; at the cap it is too late to plan the sentence.
 */
const COUNT_FROM = 0.8;

/** What one text field says about its size against its cap. */
export interface TextBudget {
  bytes: number;
  limit: number;
  /** Past the cap: the engine would refuse it. */
  over: boolean;
  /** The line under the field — a count near the cap, the refusal past it. */
  line: string | undefined;
}

/**
 * A text's size against the engine's cap, in the engine's unit (UTF-8 bytes,
 * of what is sent: the trimmed value).
 */
export function textBudget(value: string, limit: number, what: string): TextBudget {
  const bytes = utf8Bytes(value.trim());
  const over = bytes > limit;
  const line = over
    ? `${bytes} bytes — ${what} holds at most ${limit}. Shorten it, or put the long form on a page and link it here.`
    : bytes >= limit * COUNT_FROM
      ? `${bytes} of ${limit} bytes`
      : undefined;
  return { bytes, limit, over, line };
}

/** The field a draft cannot be filed without changing, and why. */
export interface Blocked {
  field: "project" | "title" | "body" | "assignee";
  reason: string;
}

/**
 * Why this draft cannot be filed yet, naming the field that holds it — the
 * first in the form's own order — or undefined when it can.
 */
export function blockedBy(draft: NewTaskDraft): Blocked | undefined {
  if (!draft.project) return { field: "project", reason: "Choose the project it is filed in." };
  if (!draft.title.trim()) {
    return { field: "title", reason: "Give the task a title — one line saying what the work is." };
  }
  const title = textBudget(draft.title, TASK_TITLE_MAX_BYTES, "a title");
  if (title.over) {
    return {
      field: "title",
      reason: `Shorten the title: it is ${title.bytes} bytes and a title holds at most ${title.limit}.`,
    };
  }
  const body = textBudget(draft.body, TASK_BODY_MAX_BYTES, "a description");
  if (body.over) {
    return {
      field: "body",
      reason: `Shorten the description: it is ${body.bytes} bytes and a description holds at most ${body.limit}.`,
    };
  }
  // WORDS IN THE ASSIGNEE FIELD THAT NO SEAT WAS TAKEN FOR: sending the create
  // without them files the task somewhere other than the form shows, and
  // sending them would be sending a typo as somebody's handle.
  const typed = draft.assigneeText.trim();
  if (!draft.assignee && typed) {
    return {
      field: "assignee",
      reason: `Choose who “${quoted(typed)}” is from the Assignee list, or clear it.`,
    };
  }
  return undefined;
}

/**
 * Typed words as a hold quotes them: whole up to a line's worth, then cut on
 * a character with an ellipsis — the sentence is about the field, and forty
 * characters say which words without the quote becoming the sentence.
 */
function quoted(words: string): string {
  const chars = [...words];
  return chars.length <= QUOTE_CHARS ? words : `${chars.slice(0, QUOTE_CHARS - 1).join("")}…`;
}

/** The fields the sheet holds, before they are sent. */
export interface NewTaskDraft {
  project: string;
  title: string;
  body: string;
  type: string;
  status: string;
  /** A handle, or "" for wherever the project sends unassigned work. */
  assignee: string;
  /**
   * What the Assignee field SHOWS: the chosen seat's name, or the words typed
   * while no seat is chosen — which hold the create ([blockedBy]) rather than
   * being dropped from it.
   */
  assigneeText: string;
  priority: string;
  /** `YYYY-MM-DD`, or "". */
  due: string;
  labels: string[];
  /** The seat the task asks, by handle, or "" for an ordinary task. */
  ask: string;
}

/**
 * The arguments one draft is filed with — ONLY what the person set.
 *
 * AN UNSET FIELD IS NOT SENT, because each has an engine default that is the
 * project's or the person's rather than this sheet's: no type is the
 * catalogue's `task`, no status is `todo`, no assignee is the project's own
 * routing, no priority is `none`. Sending the sheet's idea of each default
 * would file the same task today and a different one the day a company
 * changes its own.
 */
export function createArgs(draft: NewTaskDraft): Record<string, unknown> {
  const args: Record<string, unknown> = {
    title: draft.title.trim(),
    project: draft.project,
  };
  const body = draft.body.trim();
  if (body) args.body = body;
  if (draft.type) args.type = draft.type;
  if (draft.status) args.status = draft.status;
  if (draft.assignee) args.assignee = draft.assignee;
  if (draft.priority && draft.priority !== "none") args.priority = draft.priority;
  if (draft.due) args.due = draft.due;
  if (draft.labels.length > 0) args.labels = draft.labels;
  if (draft.ask) args.ask = draft.ask;
  return args;
}

/**
 * Which project the sheet opens on: the door's, else the ASSIGNEE's (the
 * project filed under the preset seat's own unit, or the nearest unit above it
 * that has one), else the person's own (where their create lands when it names
 * none — the engine's answer, `viewer`), else the first active project. ""
 * while none is known, and the create refuses naming the field rather than
 * guessing.
 *
 * THE ASSIGNEE'S UNIT OUTRANKS THE PERSON'S OWN because a door that names a
 * seat and no project — a seat's Message, a board lane grouped by assignee
 * across every project — is filing work FOR that seat: the Agent CEO's ask
 * belongs on the board its unit works from, not on whichever project the asker
 * happens to sit in or the first one in the list.
 */
export function startingProject(
  preset: NewTaskPreset,
  own: string,
  projects: readonly WorkProjectRow[],
  index?: OrgIndex,
): string {
  const known = (key: string | undefined) => !!key && projects.some((p) => p.key === key);
  // BEFORE THE LIST HAS ANSWERED, a named project is trusted as given: a
  // project page's door knows its key, and blanking it until a second read
  // lands would draw the sheet empty and then fill it.
  if (projects.length === 0) return preset.project || own;
  if (known(preset.project)) return preset.project!;
  const chain =
    preset.assignee && index ? (index.byHandle.get(preset.assignee)?.unit?.chain ?? []) : [];
  if (index && chain.length) {
    // KEYED ON THE UNIT ITSELF — one of this index's own — and never its name:
    // two teams sharing a name pooled their projects, and a team renamed since
    // its projects were filed matched none.
    const byUnit = projectsByUnit(projects, index);
    for (const unit of [...chain].reverse()) {
      const key = byUnit.get(unit)?.[0];
      if (key) return key;
    }
  }
  if (known(own)) return own;
  return projects[0]!.key;
}

export function NewTaskSheet({ preset, onClose }: { preset: NewTaskPreset; onClose: () => void }) {
  const nav = useNavigator();
  const viewer = useViewer();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const write = useAct("create_work_item");

  const projects = useQuery("work_projects", { limit: 200 }, { pollMs: 120_000 });
  const rows = useMemo(() => projects.data?.projects ?? [], [projects.data]);

  const [draft, setDraft] = useState<NewTaskDraft>(() => ({
    project: startingProject(preset, viewer.project, []),
    title: "",
    body: "",
    type: preset.type ?? "",
    status: preset.status ?? "",
    assignee: preset.assignee ?? "",
    assigneeText: preset.assignee
      ? (index.byHandle.get(preset.assignee)?.name ?? preset.assignee)
      : "",
    priority: preset.priority ?? "",
    due: "",
    labels: preset.labels ?? [],
    ask: preset.ask ?? "",
  }));
  const set = <K extends keyof NewTaskDraft>(key: K, value: NewTaskDraft[K]) =>
    setDraft((d) => ({ ...d, [key]: value }));

  // A DOOR THAT PRESETS A HOLDER BEFORE THE CHART HAS LOADED — a board lane
  // grouped by assignee — hands over a HANDLE, and the field opened on it.
  // When the chart arrives the seat's name replaces the handle, but only while
  // the field still shows exactly that handle. Typing un-chooses the seat, so
  // words a reader typed never meet this rule.
  const chosenName = draft.assignee ? index.byHandle.get(draft.assignee)?.name : undefined;
  useEffect(() => {
    if (!chosenName) return;
    setDraft((d) =>
      d.assignee !== "" && d.assigneeText === d.assignee && d.assigneeText !== chosenName
        ? { ...d, assigneeText: chosenName }
        : d,
    );
  }, [chosenName]);

  // THE PROJECT SETTLES ONCE THE LIST ANSWERS, and only while the person has
  // not chosen one: a project the door named that the list does not hold (an
  // archived one) falls to the person's own, and one that was never known
  // falls to the first.
  useEffect(() => {
    if (rows.length === 0) return;
    setDraft((d) =>
      rows.some((p) => p.key === d.project)
        ? d
        : { ...d, project: startingProject(preset, viewer.project, rows, index) },
    );
  }, [rows, preset, viewer.project, index]);

  const detailRead = useQuery(
    "work_project",
    { key: draft.project },
    { enabled: draft.project !== "" },
  );
  const detail = detailRead.data?.key === draft.project ? detailRead.data : undefined;

  // A LABEL OR A TYPE FROM ANOTHER PROJECT IS DROPPED when the project moves,
  // because the engine refuses a label the project does not declare — and
  // keeping it would be keeping a refusal on the form.
  useEffect(() => {
    if (!detail) return;
    setDraft((d) => {
      const tags = new Set((detail.tags ?? []).filter((t) => !t.archived).map((t) => t.slug));
      const types = new Set(detail.types.filter((t) => !t.archived).map((t) => t.slug));
      const labels = d.labels.filter((l) => tags.has(l));
      const type = d.type && !types.has(d.type) ? "" : d.type;
      return labels.length === d.labels.length && type === d.type ? d : { ...d, labels, type };
    });
  }, [detail]);

  const title = draft.title.trim();
  // A SEAT'S MESSAGE IS THIS SHEET WITH `ask` SET: one form for filing work,
  // whether it is a task or a question, so an ask carries the same project,
  // assignee and refusals a task does — and the answer lands in the Inbox.
  const asked = draft.ask ? (index.byHandle.get(draft.ask)?.name ?? draft.ask) : "";
  const titleBudget = textBudget(draft.title, TASK_TITLE_MAX_BYTES, "a title");
  const bodyBudget = textBudget(draft.body, TASK_BODY_MAX_BYTES, "a description");
  const hold = blockedBy(draft);
  const blocked = hold?.reason;
  // WHY CREATE IS HELD, whichever it is: this reader cannot file at all
  // (`useWriteAccess`), or this draft cannot be filed yet.
  const held = write.access.can ? blocked : write.access.reason;
  const titleField = useRef<HTMLInputElement>(null);
  const bodyField = useRef<HTMLTextAreaElement>(null);
  const assigneeField = useRef<HTMLInputElement>(null);
  // THE ASSIGNEE LIST IS THE SHEET'S TO OPEN, because a held Enter opens it:
  // the words holding Create are resolved by choosing from it, and a list the
  // press left closed is one more keystroke the reader has to know about.
  const [assigneeOpen, setAssigneeOpen] = useState(false);

  const submit = async () => {
    // A READER WHO CANNOT FILE, OR A PRESS ALREADY OUT, SENDS NOTHING: Enter
    // reaches here without passing the Create button's own gate, so it takes
    // that gate here — all of it but the hold, which Enter answers below.
    if (!pressable(write)) return;
    // ENTER WHILE HELD GOES TO WHAT IS HOLDING IT: the press cannot be made,
    // and the field that would make it possible is where the reader's next
    // keystroke belongs. The project is a select, which opens on its own; the
    // assignee opens its list, where the next Enter takes the first match.
    if (hold) {
      if (hold.field === "title") titleField.current?.focus();
      else if (hold.field === "body") bodyField.current?.focus();
      else if (hold.field === "assignee") {
        assigneeField.current?.focus();
        setAssigneeOpen(true);
      }
      return;
    }
    const result = await write.run(createArgs(draft), {
      done: asked
        ? `Asked ${asked} — the answer lands in your Inbox`
        : `Filed “${title}” in ${draft.project}`,
    });
    if (!result) return;
    if (result.kind === "applied") {
      onClose();
      // BY THE ADDRESS THE RECEIPT NAMES (`item`), never its key: a counter
      // restored beside work minted after it hands a new task a key an older
      // one claimed first, and that key opens the older task.
      const receipt = result.receipt as { item?: string; key?: string } | null;
      const address = receipt?.item || receipt?.key;
      if (address) nav.to(["work", address]);
    } else if (result.kind === "pending") {
      onClose();
    }
  };

  // ENTER IN A FIELD FILES THE TASK, or — held — goes to the field holding it.
  //
  // SAID HERE BECAUSE NOTHING ELSE SAYS IT. The kit's sheet is a form, and a
  // browser submits a form on Enter only through its IMPLICIT SUBMISSION,
  // which a form of several text fields performs by pressing its submit
  // button — and this one has none, since Create is a write control rather
  // than a submit button. So in a browser Enter did nothing at all, held or
  // not, while the suite, which fires `submit` on the form directly, passed.
  // Making Create the submit button would not be enough either: a held button
  // swallows its own press, so the held case — the one that needs a way to the
  // field — would still do nothing.
  //
  // A SINGLE-LINE FIELD OF THIS FORM ONLY, and only an Enter nothing else
  // took: a description takes a newline, a button presses itself, and a
  // completion list open on the assignee or the labels takes Enter as
  // choosing from it (which it marks by preventing the default). A key from a
  // PORTALLED popup — the project picker's search box — reaches this handler
  // through React's tree but is not in the form's DOM, and an Enter there that
  // matched no project is a search, never a press.
  const onFieldEnter = (event: KeyboardEvent<HTMLElement>) => {
    if (
      event.key !== "Enter" ||
      event.nativeEvent.defaultPrevented ||
      event.nativeEvent.isComposing ||
      event.altKey ||
      event.ctrlKey ||
      event.metaKey ||
      event.shiftKey ||
      !(event.target instanceof HTMLInputElement) ||
      !event.currentTarget.contains(event.target)
    ) {
      return;
    }
    event.preventDefault();
    void submit();
  };

  return (
    <Modal
      open
      variant="sheet"
      stackBody
      title={asked ? `Ask ${asked}` : "New task"}
      icon={<PlusGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      // THE FOOT SAYS WHAT THE PRESS WILL DO, or why it cannot be made — beside
      // the button, where the eye is when it presses. The kit writes a held
      // button's reason for a screen reader only, so a sighted reader saw a
      // Create that did nothing and no word about why. Unheld, it says who the
      // task is filed as: the sheet head used to, under the title, and a
      // second line there sat 5px from the head's top edge, since the kit's
      // sheet head is the page bar's height and sized for a title alone (the
      // org builder's sheet carries none either).
      footerStart={
        held ? (
          <span className="new-task-hold">{held}</span>
        ) : viewer.name ? (
          <span>
            {asked
              ? `Asked as ${viewer.name} — the answer lands in your Inbox`
              : `Filed as ${viewer.name}`}
          </span>
        ) : undefined
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
            blocked={blocked}
            onPress={() => void submit()}
          >
            {asked ? "Send" : "Create task"}
          </WriteButton>
        </>
      }
    >
      {/* THE FIELDS AS ONE KEYBOARD REGION, drawn as no box of its own
          (`display: contents`), so the sheet's stacked body lays them out
          exactly as before. */}
      <div className="new-task-fields" onKeyDown={onFieldEnter}>
        <FormField label="Project" htmlFor="new-task-project">
          <Select
            id="new-task-project"
            ariaLabel="Project"
            searchable={rows.length > 8}
            searchPlaceholder="Find a project"
            placeholder={projects.loading ? "Reading the projects…" : "Choose a project"}
            value={draft.project || undefined}
            options={projectOptions(rows, draft.project)}
            onChange={(next) => set("project", String(next))}
          />
        </FormField>

        <FormField
          label="Title"
          htmlFor="new-task-title"
          helper={titleBudget.over ? undefined : titleBudget.line}
          error={titleBudget.over ? titleBudget.line : undefined}
        >
          {(field) => (
            <Input
              ref={titleField}
              id={field.id}
              aria-describedby={field.describedBy}
              error={titleBudget.over}
              value={draft.title}
              autoFocus
              placeholder={asked ? "What you want to ask" : "What needs doing"}
              onChange={(event) => set("title", event.target.value)}
            />
          )}
        </FormField>

        <FormField
          label="Description"
          optional
          htmlFor="new-task-body"
          // THE COUNT REPLACES THE HINT only while it is a count; past the cap
          // the hint stays and the refusal is the error line under it.
          helper={
            !bodyBudget.over && bodyBudget.line
              ? bodyBudget.line
              : "Markdown. What is wanted, why, and how anyone would know it is done."
          }
          error={bodyBudget.over ? bodyBudget.line : undefined}
        >
          {(field) => (
            <Textarea
              ref={bodyField}
              id={field.id}
              aria-describedby={field.describedBy}
              error={bodyBudget.over}
              rows={5}
              value={draft.body}
              onChange={(event) => set("body", event.target.value)}
            />
          )}
        </FormField>

        <div className="new-task-pair">
          <FormField label="Type" htmlFor="new-task-type">
            <Select
              id="new-task-type"
              ariaLabel="Type"
              value={draft.type || DEFAULT_TYPE}
              options={typeOptions(detail)}
              onChange={(next) => set("type", String(next) === DEFAULT_TYPE ? "" : String(next))}
            />
          </FormField>
          <FormField label="Status" htmlFor="new-task-status">
            <Select
              id="new-task-status"
              ariaLabel="Status"
              value={draft.status || "todo"}
              options={STATUSES.map((s) => ({
                value: s.value,
                label: statusLabel(s.value, detail?.statuses),
              }))}
              onChange={(next) => set("status", String(next) === "todo" ? "" : String(next))}
            />
          </FormField>
        </div>

        <AssigneeField
          seats={index.seats}
          value={draft.assignee}
          text={draft.assigneeText}
          onChange={({ assignee, text }) =>
            setDraft((d) => ({ ...d, assignee, assigneeText: text }))
          }
          open={assigneeOpen}
          onOpenChange={setAssigneeOpen}
          fieldRef={assigneeField}
          landing={landingOf(detail, index.byHandle)}
        />

        <div className="new-task-pair">
          <FormField label="Priority" htmlFor="new-task-priority">
            <Select
              id="new-task-priority"
              ariaLabel="Priority"
              value={draft.priority || "none"}
              options={PRIORITIES.map((p) => ({
                value: p,
                label: p === "none" ? "No priority" : humanize(p),
              }))}
              onChange={(next) => set("priority", String(next))}
            />
          </FormField>
          <FormField label="Due" optional htmlFor="new-task-due">
            <Input
              id="new-task-due"
              type="date"
              value={draft.due}
              onChange={(event) => set("due", event.target.value)}
            />
          </FormField>
        </div>

        <FormField
          label="Labels"
          optional
          htmlFor="new-task-labels"
          helper={
            detail && (detail.tags ?? []).filter((t) => !t.archived).length === 0
              ? `${draft.project} declares no labels yet.`
              : "The project's own labels — one it does not declare is refused."
          }
        >
          {/* THE HELP LINE — which labels the project takes — is the control's
            description, for the reason the assignee's is. */}
          {(field) => (
            <TagsInput
              id={field.id}
              aria-label="Labels"
              aria-describedby={field.describedBy}
              value={draft.labels}
              onChange={(next) => set("labels", next)}
              options={(detail?.tags ?? [])
                .filter((t) => !t.archived)
                .map((t) => ({ value: t.slug, label: t.label }))}
              disabled={!detail}
              placeholder={detail ? "Add a label" : "Choose a project first"}
            />
          )}
        </FormField>

        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/** The projects as the Project field offers them: key and name, archived ones out. */
function projectOptions(rows: readonly WorkProjectRow[], chosen: string): SelectOption[] {
  const options: SelectOption[] = rows.map((p) => ({
    value: p.key,
    label: p.name ? `${p.key} · ${p.name}` : p.key,
    text: `${p.key} ${p.name}`,
  }));
  // A PROJECT THE DOOR NAMED BEFORE THE LIST ANSWERED is still a value the
  // field holds, and a select whose value is not among its options draws a
  // blank — so it is offered as itself until the list says otherwise.
  if (chosen && !rows.some((p) => p.key === chosen))
    options.unshift({ value: chosen, label: chosen });
  return options;
}

/** The project's types, its catalogue's words, the archived ones out. */
function typeOptions(detail: WorkProjectDetail | undefined): SelectOption[] {
  const types = (detail?.types ?? []).filter((t) => !t.archived);
  if (types.length === 0) return [{ value: DEFAULT_TYPE, label: typeName(DEFAULT_TYPE) }];
  return types.map((t) => ({
    value: t.slug,
    label: typeName(t.slug, detail?.types),
    description: t.description || undefined,
  }));
}

/**
 * Where an unassigned task goes, in the project the sheet has chosen: its
 * default assignee, else triage for its lead. Said before the press, because
 * "leave it empty" is only a choice if the reader knows what empty does.
 */
function landingOf(
  detail: WorkProjectDetail | undefined,
  byHandle: ReadonlyMap<string, Seat>,
): string {
  if (!detail) return "Left empty, it lands where the project sends unassigned work.";
  const name = (handle: string) => byHandle.get(handle)?.name ?? handle;
  // A DEFAULT THE CHART NO LONGER HOLDS IS NOT WHERE IT GOES: the engine
  // files such a task to nobody and puts it in triage, warning as it does
  // (`tracker.Writer.landsOn`) — so promising the departed seat here would be
  // a sentence the create contradicts. The same test the engine makes: the
  // handle, exactly, on the chart this screen loaded.
  if (detail.default_assignee && !byHandle.has(detail.default_assignee)) {
    return `Left empty, it lands in triage: ${detail.key}'s default assignee, ${detail.default_assignee}, is no longer on the org chart.`;
  }
  if (detail.default_assignee) {
    return `Left empty, it goes to ${name(detail.default_assignee)}, ${detail.key}'s default assignee.`;
  }
  if (detail.lead.handle) {
    return `Left empty, it lands in triage and ${name(detail.lead.handle)}, who leads ${detail.key}, is told.`;
  }
  return `Left empty, it lands in ${detail.key}'s triage — and nobody leads ${detail.key} to route it.`;
}

/**
 * The assignee: a seat, chosen by typing what you remember of them.
 *
 * THE CHART COMPLETES, THE ENGINE SUGGESTS. Local completion over names and
 * handles is instant and covers the usual case; the engine's `colleague`
 * answer is what a partial or remembered-wrong name resolves to, and when it
 * names exactly one seat that seat leads the list. The field holds WORDS and
 * the task holds a HANDLE: what is typed is not an assignee until a seat is
 * taken from the list, so a typo is never sent as somebody's handle — and
 * words left unchosen hold the create ([blockedBy]), so they are never
 * silently dropped from it either.
 *
 * THE NAME IS WHAT A ROW IS READ FOR, so the engine's row carries a hint as
 * short as every other row's ("@handle · best match") and the tier that
 * matched is said where there is room for it: in the row's accessible name,
 * and on the field's help line once that seat is taken. The kit's hint does
 * not shrink and its label does, so the tier written into the hint — "the
 * engine's match: part of the name matches" — took the whole row and drew
 * the seat's name as "Age…", or not at all, in exactly the one case the row
 * exists for.
 */
function AssigneeField({
  seats,
  value,
  text,
  onChange,
  open,
  onOpenChange,
  fieldRef,
  landing,
}: {
  seats: readonly Seat[];
  /** The chosen seat's handle, or "". */
  value: string;
  /** What the field shows. */
  text: string;
  onChange: (next: { assignee: string; text: string }) => void;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  fieldRef: RefObject<HTMLInputElement | null>;
  landing: string;
}) {
  // NO SEAT IS CHOSEN BY AN EMPTY VALUE, even one the chart lists without a
  // handle — or the field would say a seat "is woken with it" while nothing
  // is sent.
  const chosen = value ? seats.find((s) => s.handle === value) : undefined;
  const term = text.trim();
  const unchosen = value === "" && term !== "";
  // ONLY WHILE THE WORDS ARE NOT ALREADY THE CHOSEN SEAT: asking the engine
  // who "Agent SWE" is after the reader took Agent SWE is a read for nothing.
  const asking = open && colleagueSendable(term) && term !== chosen?.name;
  const colleague = useQuery("colleague", { q: term }, { enabled: asking });
  // WHY THE ENGINE OFFERED THE SEAT THAT WAS TAKEN, kept from the press: once
  // the field holds the seat's name the engine is no longer asked, and the
  // words it matched are gone from the field.
  const [matched, setMatched] = useState<{ handle: string; words: string; why: string }>();

  const suggested = asking ? colleague.data?.match : undefined;
  const top = suggested ? seats.find((s) => s.handle === suggested.handle) : undefined;
  const options = useMemo<ComboboxOption[]>(() => {
    const needle = term.toLowerCase();
    const local = seats
      .filter((s) => s.handle && s !== top)
      .filter(
        (s) =>
          needle === "" ||
          s.name.toLowerCase().includes(needle) ||
          s.handle.toLowerCase().includes(needle),
      )
      .slice(0, SEAT_CHOICES - (top ? 1 : 0));
    const row = (s: Seat, hint: ReactNode): ComboboxOption => ({
      value: s.handle,
      label: s.name,
      hint,
    });
    return [
      ...(top && suggested
        ? [
            row(
              top,
              <>
                {handleLabel(top.handle)} · best match
                <span className="sr-only">, the engine's match: {suggested.why}</span>
              </>,
            ),
          ]
        : []),
      ...local.map((s) =>
        row(s, `${handleLabel(s.handle)} · ${s.kind === "human" ? "person" : "agent"}`),
      ),
    ];
  }, [seats, term, top, suggested]);

  // THE TIER IS SAID ONLY FOR THE SEAT IT WAS SAID ABOUT: a reader who took
  // the engine's match and then another seat is not told why the first one
  // matched.
  const why = matched && matched.handle === value ? matched : undefined;

  return (
    <FormField
      label="Assignee"
      optional
      htmlFor="new-task-assignee"
      // WHAT THE FIELD WILL DO, in the three states it can be in: a seat is
      // chosen (named from the chart, or by the handle a door preset while the
      // chart is still loading), words are typed and nothing is chosen yet,
      // or it is empty — and only the last is where "left empty" is true.
      helper={
        value
          ? `${chosen?.name ?? handleLabel(value)} is woken with it and follows it.${
              why ? ` The engine's match for “${quoted(why.words)}”: ${why.why}.` : ""
            }`
          : unchosen
            ? "Not chosen yet — take the seat from the list. Typed words are never sent as a handle."
            : landing
      }
    >
      {/* THE HELP LINE IS THE CONTROL'S DESCRIPTION, through the field's own
          wiring: drawn beside a control that did not name it, the line saying
          where an empty field sends the task — and now that typed words are
          not chosen — was never read to anybody who could not see it. */}
      {(field) => (
        <Combobox
          ref={fieldRef}
          id={field.id}
          aria-describedby={field.describedBy}
          label="Assignee suggestions"
          value={text}
          placeholder="Type a name or a handle"
          options={options}
          // OPEN WITH NOTHING TO OFFER is a list that says so — a typo that
          // closed the list silently read as a field that had stopped
          // completing.
          open={open}
          onOpenChange={onOpenChange}
          // BACK IN A FIELD HOLDING WORDS NOBODY WAS TAKEN FOR, the list of
          // who they might mean is what the reader came back for.
          onFocus={() => {
            if (unchosen) onOpenChange(true);
          }}
          onValueChange={(next) => {
            // WORDS ARE NOT A HANDLE: editing them un-chooses the seat.
            onChange({ assignee: "", text: next });
            onOpenChange(true);
          }}
          onCommit={(option) => {
            const seat = seats.find((s) => s.handle === option.value);
            setMatched(
              top && suggested && option.value === top.handle
                ? { handle: top.handle, words: term, why: suggested.why }
                : undefined,
            );
            onChange({ assignee: option.value, text: seat?.name ?? option.value });
            onOpenChange(false);
          }}
          emptyMessage={
            seats.length === 0 ? "The org chart has not loaded yet" : `No seat is called “${term}”`
          }
          // THE CLEAR CONTROL IS THE KIT'S OWN, INSIDE THE FIELD, as a search
          // box draws it — which this is, over the chart. Beside the field it
          // was a column the completion list did not span, so the help line
          // under the field showed its last words beside the open list; inside
          // it, the list is as wide as everything it hangs from. It is drawn
          // only while there is something to clear, is named for what it
          // clears, and hands focus back to the field it emptied.
          onClear={() => onChange({ assignee: "", text: "" })}
          clearLabel="Clear assignee"
        />
      )}
    </FormField>
  );
}
