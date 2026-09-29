/**
 * The node editor: the company's charter, a unit, or a seat, in a side sheet.
 *
 * FORM-LOCAL, APPLIED AS ONE OPERATION. Every field is held in a local form
 * until Apply, which records the fields that changed as one `edit` (see
 * `editorForm.ts`): nothing reaches the draft while a sentence is half
 * typed, a refusal keeps the form open with the engine's reason on screen,
 * and Undo takes back the whole of what Apply did. Closing a form with
 * changes in it, by Cancel, Close, Escape or the veil, asks first, because a
 * form's changes exist nowhere else, and so does every move that leaves the
 * Builder lens (one of its links, Back or Forward, a reload): the form holds
 * a leave guard (`app/router.useLeaveGuard`) for as long as it has changes.
 * A move within the lens (Back from the outline to the canvas) keeps the
 * editor open with its form, so it is let go without a question
 * (`BuilderContext.keepsTheLens`).
 *
 * WHAT IS EDITABLE IS DECIDED FIELD BY FIELD (the field coverage of the
 * builder's spec). A field the builder can write is a field. A field it shows
 * but cannot write is a read-only fact that says why and where it is written
 * instead: a node's RUNTIME half is carried by the chart opaquely, and the
 * builder draws the few runtime fields it edits and states the rest. Fields
 * that are neither are not drawn: the chart keeps them, and the builder
 * preserves every key it does not model.
 *
 * AN ADDRESS IS A FIELD. A seat's handle and a unit's key are what every
 * reference names them by, and a new one on a node the chart holds is the
 * chart's RENAME, which keeps the node — its identity, memory and mailbox —
 * and leaves the old address reaching it until something else takes it.
 *
 * A RUNTIME HALF THE READER WAS NOT SHOWN IS NOT DRAWN. The chart serves it
 * only to a reader holding the grant that reads the configuration and says
 * when it withheld it, so the fields that live there are left out with a
 * sentence saying why, rather than drawn empty and read as absent.
 *
 * A FIELD FOR A TOOL IS DRAWN ONLY WHERE THE TOOL IS. A seat's GitHub tier,
 * Mattermost channel and GitLab access level mean something only when the
 * company has connected that tool, so each says "<Tool> is not connected"
 * instead of offering a setting that does nothing. Two of them change more than their field, and say so
 * beside it: a GitHub tier on a seat with no GitHub block enrols the seat,
 * and a tier change on a seat whose app exists does not reach the app, whose
 * permissions were fixed when it was created.
 *
 * NO CREDENTIAL IS RENDERED. Tool credentials appear as server and variable
 * names, a seat's chat app blocks as the settings a person writes (a channel,
 * a username), and nothing else of theirs.
 *
 * THE DRAFT'S PROBLEMS SIT BESIDE THE FIELDS THEY NAME, placed by the field
 * path the check placed them on; whatever names no drawn field is listed at
 * the top, so nothing the check said is dropped. A warning, which a save
 * accepts, is listed at the top as a caution rather than drawn as a field's
 * error (see `placeOnFields`).
 */

import { useState, type ReactNode } from "react";
import { useLeaveGuard } from "~/app/router.tsx";
import type { CompanyDocument } from "~/protocol/index.ts";
import { REDACTED, plural } from "~/lib/format.ts";
import { ConfigField, type FieldChoice } from "~/components/ConfigField.tsx";
import {
  keepsTheLens,
  useBuilder,
  type BuilderApi,
  type EditorSectionName,
} from "./BuilderContext.tsx";
import {
  companyForm,
  companyParts,
  editIntent,
  providerKeys,
  renames,
  scheduleRuns,
  schedulesOf,
  seatForm,
  seatParts,
  tokenBudgetError,
  unitForm,
  unitParts,
  type CompanyForm,
  type SeatForm,
  type UnitForm,
} from "./editorForm.ts";
import {
  ACKNOWLEDGEMENT_TEXT,
  ADDRESS_HELP,
  EditorSection,
  HueFact,
  NAME_HELP,
  NodeProblems,
  NotConnected,
  ReadOnlyFact,
  Refusal,
  RUNTIME_ELSEWHERE,
  RuntimeHidden,
  ScreenLink,
  UnitTypeField,
  placeOnFields,
} from "./dialogParts.tsx";
import { NodeGlyph, type NodeGlyphKind } from "./nodeMarks.tsx";
import type { Segment } from "./model/document.ts";
import {
  allSeats,
  allUnits,
  locate,
  type DraftSeat,
  type DraftUnit,
  type SeatData,
  type UnitData,
} from "./model/draft.ts";
import { getPath, isRecord, jsonEqual } from "./model/json.ts";
import { COMPANY_KEY, handleOfKey, isMintedKey, type NodeKey } from "./model/keys.ts";
import { kindOf, type EditPartIntent } from "./model/operations.ts";
import type { PlacedProblem } from "./model/problems.ts";
import { recordIntent } from "./model/reducer.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import { datadogEnabled, datadogFallback, effectiveLeads } from "./chartModel.ts";
import {
  PHASE_MODEL_FIELDS,
  defaultGitLabAccessLevel,
  derivedSeatOf,
  gitLabAccessLevel,
  hasGitLabProvisioning,
  isConnected,
  isWholeReference,
  mattermostBotUsername,
  nameOfHandle,
  placementSummary,
  providerOrder,
  savedDerivation,
  toolCredentialNames,
  unpinnedProvider,
} from "./nodeFacts.ts";
import {
  Button,
  Callout,
  Checkbox,
  ConfirmModal,
  EmptyState,
  FormField,
  InlineCode,
  ListInput,
  Modal,
  TagsInput,
} from "@crewlethq/ui";
import type { TagsInputOption } from "@crewlethq/ui";
import { withProblems } from "~/ui/Problems.tsx";

export function NodeEditor({
  nodeKey,
  section,
  onClose,
}: {
  nodeKey: NodeKey;
  /** The part of the form to start on (see `EditorSectionName`). */
  section?: EditorSectionName;
  onClose: () => void;
}) {
  const { state } = useBuilder();
  if (nodeKey === COMPANY_KEY) return <CompanyEditor onClose={onClose} />;
  const found = locate(state.draft, nodeKey);
  if (!found) {
    return (
      <Modal open variant="sheet" stackBody title="Edit" onClose={onClose}>
        <EmptyState
          title="This node is no longer in the draft"
          description="It was removed by an undo or an update onto a newer revision. Close this editor and choose another node."
        />
      </Modal>
    );
  }
  // ONE FORM PER OPENING, NOT PER KEY. The Builder mounts a new editor for
  // every opening (its dialog host keys each dialog by the opening), so
  // another node, or the same one opened again, starts from its own data. A
  // node's key can also change while its editor is open (a save moves the
  // nodes it created onto the addresses the chart gave them), and the form and
  // its drawer stay: keyed by the node here, they would be torn down and built
  // again on that answer.
  return found.kind === "unit" ? (
    <UnitEditor unit={found.node} section={section} onClose={onClose} />
  ) : (
    <SeatEditor seat={found.node} section={section} onClose={onClose} />
  );
}

// ---------------------------------------------------------------------------
// The shell every form shares
// ---------------------------------------------------------------------------

/** A form that starts from `initial` and knows whether it has been changed. */
function useForm<T>(initial: () => T) {
  const [start] = useState(initial);
  const [form, setForm] = useState(start);
  return {
    initial: start,
    form,
    set: (patch: Partial<T>) => setForm((was) => ({ ...was, ...patch })),
    dirty: !jsonEqual(start, form),
  };
}

/**
 * Records a form's parts as one operation through the reducer's own door,
 * and closes the editor only when it recorded, so a refusal stays on screen.
 */
function useApply(api: BuilderApi, target: NodeKey, onClose: () => void) {
  const [refusal, setRefusal] = useState<string | null>(null);
  const apply = (parts: readonly EditPartIntent[]) => {
    if (parts.length === 0) return onClose();
    const intent = editIntent(target, parts);
    const answer = recordIntent(api.state, intent);
    if (!answer.ok) {
      if (answer.refusal === "no_change") return onClose();
      setRefusal(answer.message);
      return;
    }
    api.dispatch({ type: "record", intent });
    onClose();
  };
  return { refusal, apply };
}

/** The problems and warnings the last check of the current draft placed on a node. */
function placedOn(api: BuilderApi, key: NodeKey): PlacedProblem[] {
  const current = new Set<unknown>([...api.problemsFor(key), ...api.warningsFor(key)]);
  return (api.state.check.problems.byNode.get(key) ?? []).filter((p) => current.has(p.source));
}

/**
 * Why Apply is unavailable when it is the posture rather than the form.
 *
 * NOT THE BANNER'S OWN SENTENCE. The callout at the top of the panel says the
 * organization cannot be changed right now; repeating it on the button reads
 * it out twice to anybody using a screen reader, once on entering the form
 * and again on reaching the control.
 */
const READ_ONLY_REASON = "These fields are for reading, so there is nothing to apply.";

function EditorShell({
  title,
  mark,
  name,
  dirty,
  blocked,
  readOnly,
  refusal,
  problems,
  onApply,
  onClose,
  children,
}: {
  title: string;
  /** What is being edited, for the panel's own mark: see `NodeGlyph`. */
  mark: NodeGlyphKind;
  /** How the node is named in the discard prompt. */
  name: string;
  dirty: boolean;
  /** Why Apply is unavailable for what the form holds, or `null`. */
  blocked: string | null;
  readOnly: boolean;
  refusal: string | null;
  problems: readonly PlacedProblem[];
  onApply: () => void;
  onClose: () => void;
  children: ReactNode;
}) {
  // `leave` is the move the router held (a link, Back or Forward) when the
  // question came from one, so the same question serves a close and a move,
  // and discarding then makes the move. Only a move off the lens is held: the
  // Builder keeps this editor, form and all, through a move within it.
  const [confirming, setConfirming] = useState<{ leave: (() => void) | null } | null>(null);
  const requestClose = () => (dirty ? setConfirming({ leave: null }) : onClose());
  useLeaveGuard(
    dirty
      ? (to, leave) => {
          if (keepsTheLens(to)) return false;
          setConfirming({ leave });
          return true;
        }
      : null,
  );
  const discard = () => {
    const leave = confirming?.leave ?? null;
    onClose();
    leave?.();
  };
  const disabled = readOnly || blocked !== null;
  const reason = readOnly ? READ_ONLY_REASON : (blocked ?? undefined);
  return (
    <>
      <Modal
        open
        variant="sheet"
        stackBody
        title={title}
        // THE NODE'S OWN MARK, not a pencil: the same glyph the table row and
        // the chart card carry for this node, so the three surfaces mark it
        // identically and the panel says what it is editing before its title
        // is read.
        icon={<NodeGlyph kind={mark} size="md" />}
        onClose={requestClose}
        // ALWAYS A FORM. The panel is a <form> only while it has a submit
        // handler, and swapping the element as Apply became unavailable would
        // remount every field in it, dropping focus and a list's edit in
        // progress. The guard lives in the handler instead.
        onSubmit={() => {
          if (!disabled) onApply();
        }}
        // COMMITTED FROM THE HEAD, as the console's edit panel is. A seat's
        // form is a column a reader scrolls (1,764px against a 794px window
        // on a seat with a dozen fields), and Apply at the foot of it is a
        // button they have to travel the whole form to reach and travel back
        // from to see the field they were applying. With Cancel in the band
        // there is no close control beside it doing the same thing.
        showCloseButton={false}
        headerActions={
          <>
            <Button variant="secondary" size="small" onClick={requestClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="small"
              type="submit"
              /*
               * SOFT, NOT NATIVE. A natively disabled button takes no focus
               * and no hover, so the reason it cannot be pressed is
               * unreachable by exactly the reader who needs it; this says the
               * same thing to a screen reader and leaves the control where it
               * was. The reason is the one in the foot, said again where the
               * control is: a reader who tabs straight to Apply never passes
               * the line at the other end of the band.
               */
              disabledReason={disabled ? reason : undefined}
            >
              Apply
            </Button>
          </>
        }
        /*
         * WHY APPLY REFUSES, DRAWN, at the foot's own start edge. It used to
         * be a hand-rolled flexing span inside the actions, which put it 130px
         * from Cancel with the band's first 257px empty, because the end
         * slot's own auto margin and a `flex: 1 1 auto` child were two spacing
         * mechanisms in one band. The button says the same sentence out of
         * view, which is not a second statement of it: a reader who tabs
         * straight to Apply never passes this line at the other end.
         */
        footerStart={disabled ? reason : undefined}
      >
        {readOnly && (
          <Callout variant="neutral">
            The organization cannot be changed right now, so these fields are for reading.
          </Callout>
        )}
        <Refusal message={refusal} />
        <NodeProblems problems={problems} />
        {children}
      </Modal>
      {/*
       * ONE QUESTION, ONE ANSWER, and nothing around it: the prompt shape,
       * which is what the console draws here too. `destructive` makes it an
       * alertdialog, so a reader's software announces the whole prompt rather
       * than its name alone: the name asks about discarding and the sentence
       * is the consequence.
       */}
      <ConfirmModal
        open={confirming !== null}
        destructive
        title="Discard your changes?"
        message={
          confirming?.leave === undefined || confirming.leave === null
            ? `The changes to ${name} have not been applied to the draft. Discarding them leaves the draft as it was.`
            : `The changes to ${name} have not been applied to the draft, and you are leaving the builder. Discarding them leaves the draft as it was and goes on to where you were going.`
        }
        cancelLabel="Keep editing"
        confirmLabel="Discard changes"
        onClose={() => setConfirming(null)}
        onConfirm={discard}
      />
    </>
  );
}

/** A schedule's toggle, with what it runs and when. */
function ScheduleToggles({
  data,
  unit,
  values,
  onChange,
  disabled,
  error,
}: {
  data: SeatData | UnitData;
  unit: boolean;
  values: Readonly<Record<string, boolean>>;
  onChange: (name: string, enabled: boolean) => void;
  disabled: boolean;
  /** What the engine said about this node's schedules. */
  error: string | undefined;
}) {
  const schedules = schedulesOf(data);
  if (schedules.length === 0) return null;
  return (
    <EditorSection
      title="Schedules"
      hint={
        <>
          Schedules are written in the runtime half, which crewlet config import writes from a
          company file. Here each can be switched on or off.{" "}
          <ScreenLink to="schedules">Open Schedules</ScreenLink>
        </>
      }
    >
      {schedules.map((s) => {
        const runner = !unit
          ? ""
          : s.target === "lead"
            ? ", run by the unit lead"
            : ", run by each agent member";
        return (
          <Checkbox
            key={s.name}
            label={`Enabled: ${s.name}`}
            description={`${s.cron}${s.timezone ? ` (${s.timezone})` : ""}${runner}. ${s.task}`}
            checked={values[s.name] ?? scheduleRuns(s)}
            onCheckedChange={(checked) => onChange(s.name, checked)}
            disabled={disabled}
          />
        );
      })}
      {error && (
        <Callout variant="danger" role="alert">
          {error}
        </Callout>
      )}
    </EditorSection>
  );
}

/** Tool credentials as names, never values. */
function ToolCredentialFact({ data }: { data: SeatData | UnitData }) {
  const names = toolCredentialNames(data);
  if (names.length === 0) return null;
  return (
    <ReadOnlyFact
      label="Tool credentials"
      reason={`They name servers the settings' mcp_servers block defines. ${RUNTIME_ELSEWHERE} Values are never shown.`}
    >
      <ul className="builder-list">
        {names.map(({ server, variables }) => (
          <li key={server}>
            <InlineCode>{server}</InlineCode>
            {variables.length > 0 && ` (${variables.join(", ")})`}
          </li>
        ))}
      </ul>
    </ReadOnlyFact>
  );
}

/**
 * The tracker project and knowledge-base space a node works in: where
 * unrouted work for it goes and where it files its own. Not a permission, and
 * the same field whichever backend the company runs its tracker and knowledge
 * base on — the engine's own, Jira or Confluence.
 *
 * A RELATION, which the chart asks the configuration grant for: authority is
 * derived from it (a lead of a project leads its work), so a lead who could
 * change it could take over another team's project. The save names the fields
 * when a change is refused for that.
 */
function Owns({
  who,
  project,
  space,
  onProject,
  onSpace,
  errorFor,
  disabled,
}: {
  who: "seat" | "unit";
  project: string;
  space: string;
  onProject: (next: string) => void;
  onSpace: (next: string) => void;
  errorFor: (path: readonly Segment[]) => string | undefined;
  disabled: boolean;
}) {
  return (
    <EditorSection
      title="Owns"
      hint={`Where unrouted work for this ${who} goes and where it files its own. Not a permission. Changing either takes the grant that writes the company's configuration.`}
    >
      <ConfigField
        label="Project"
        kind="id"
        value={project}
        onChange={onProject}
        required={false}
        disabled={disabled}
        error={errorFor(["project"])}
      />
      <ConfigField
        label="Knowledge space"
        kind="id"
        value={space}
        onChange={onSpace}
        required={false}
        disabled={disabled}
        error={errorFor(["space"])}
      />
    </EditorSection>
  );
}

// ---------------------------------------------------------------------------
// The company
// ---------------------------------------------------------------------------

const COMPANY_FIELDS: Segment[][] = [["name"], ["mission"], ["vision"], ["policies"]];

function CompanyEditor({ onClose }: { onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const { initial, form, set, dirty } = useForm<CompanyForm>(() =>
    companyForm(state.draft.company),
  );
  const { refusal, apply } = useApply(api, COMPANY_KEY, onClose);
  const [acknowledged, setAcknowledged] = useState(false);
  const { errorFor, rest } = placeOnFields(placedOn(api, COMPANY_KEY), COMPANY_FIELDS);

  const disabled = api.readOnly;

  // ASKED OF THE FORM THAT RENAMES, AND ONLY THEN. The question is about what
  // this Apply does, so a name nobody typed in (a draft renamed by an earlier
  // edit, a stored name with a space around it) asks nothing of a mission
  // edit. It compares with the SAVED name as the engine holds it, untrimmed,
  // because the engine derives agent ids from exactly that string, and a
  // name typed back to the saved one is no rename at all.
  const savedName = typeof state.base.settings?.name === "string" ? state.base.settings.name : "";
  const renaming =
    savedName !== "" && renames(initial.name, form.name) && form.name.trim() !== savedName;
  const blocked =
    form.name.trim() === ""
      ? "The company needs a name."
      : renaming && !acknowledged
        ? "Confirm what renaming the company does."
        : null;

  return (
    <EditorShell
      title="Edit the charter"
      mark="company"
      name="the charter"
      dirty={dirty}
      blocked={blocked}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      onApply={() => apply(companyParts(initial, form))}
      onClose={onClose}
    >
      <ConfigField
        label="Company name"
        value={form.name}
        onChange={(name) => set({ name })}
        disabled={disabled}
        error={errorFor(["name"])}
      />
      {renaming && (
        <Checkbox
          framed
          tone="danger"
          label="I understand what renaming the company does"
          description={ACKNOWLEDGEMENT_TEXT.company_rename}
          checked={acknowledged}
          onCheckedChange={setAcknowledged}
          disabled={disabled}
        />
      )}
      <ConfigField
        label="Mission"
        kind="multiline"
        value={form.mission}
        onChange={(mission) => set({ mission })}
        required={false}
        disabled={disabled}
        error={errorFor(["mission"])}
      />
      <ConfigField
        label="Vision"
        kind="multiline"
        value={form.vision}
        onChange={(vision) => set({ vision })}
        required={false}
        disabled={disabled}
        error={errorFor(["vision"])}
      />
      <ListInput
        label="Policies"
        itemName="policy"
        multiline
        value={form.policies}
        onChange={(policies) => set({ policies })}
        optional
        disabled={disabled}
        error={withProblems(errorFor(["policies"]))}
      />
    </EditorShell>
  );
}

// ---------------------------------------------------------------------------
// A unit
// ---------------------------------------------------------------------------

/**
 * The field paths a unit's form draws, so a problem placed on a field this
 * form does NOT draw is listed at the top rather than attached to a field
 * nobody can see.
 */
function unitFieldPaths(data: UnitData, runtimeVisible: boolean): Segment[][] {
  return [
    ["name"],
    ["key"],
    ["type"],
    ["purpose"],
    ["lead"],
    ["goals"],
    ["channel"],
    ["knowledge_refs"],
    ["project"],
    ["space"],
    ...(runtimeVisible && schedulesOf(data).length > 0
      ? [["runtime", "schedules"] as Segment[]]
      : []),
  ];
}

/** The draft unit a unit's key names, and the unit above it. */
function parentUnitOf(api: BuilderApi, key: NodeKey): DraftUnit | undefined {
  const found = locate(api.state.draft, key);
  if (!found || found.parent === COMPANY_KEY) return undefined;
  const parent = locate(api.state.draft, found.parent);
  return parent?.kind === "unit" ? parent.node : undefined;
}

function UnitEditor({
  unit,
  section,
  onClose,
}: {
  unit: DraftUnit;
  section: EditorSectionName | undefined;
  onClose: () => void;
}) {
  const api = useBuilder();
  const { state } = api;
  const key = unit.key;
  const { initial, form, set, dirty } = useForm<UnitForm>(() => unitForm(unit.data));
  const { refusal, apply } = useApply(api, key, onClose);
  const runtimeVisible = state.base.runtimeVisible;
  const { errorFor, rest } = placeOnFields(
    placedOn(api, key),
    unitFieldPaths(unit.data, runtimeVisible),
  );
  const disabled = api.readOnly;
  const saved = !isMintedKey(key);

  // What the unit inherits when it declares nothing: the lead and channel the
  // unit above it resolves to (`chartModel.effectiveLeads`).
  const parent = parentUnitOf(api, key);
  const above = parent ? effectiveLeads(state.draft).get(parent.key) : undefined;
  const inheritedLead = above?.lead ? seatLabel(state.draft, above.lead) : "";
  const inheritedChannel = above?.channel ?? "";

  const leadChoices: FieldChoice[] = [
    {
      value: "",
      label:
        inheritedLead && parent
          ? `No lead (inherits ${inheritedLead} from ${parent.data.name})`
          : "No lead",
    },
    ...seatChoices(state.draft),
  ];
  // A declared lead no seat of the draft holds is still the current answer.
  if (form.lead !== "" && !leadChoices.some((c) => c.value === form.lead)) {
    leadChoices.push({ value: form.lead, label: `@${form.lead} (names no seat)` });
  }

  const blocked =
    form.name.trim() === ""
      ? "A unit needs a name."
      : form.key.trim() === ""
        ? "A unit needs a key."
        : null;
  const renamed = saved && renames(initial.name, form.name);

  return (
    <EditorShell
      title={`Edit ${unit.data.name || "unit"}`}
      mark="unit"
      name={unit.data.name || "this unit"}
      dirty={dirty}
      blocked={blocked}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      onApply={() => apply(unitParts(key, initial, form))}
      onClose={onClose}
    >
      <ConfigField
        label="Name"
        value={form.name}
        onChange={(name) => set({ name })}
        disabled={disabled}
        help={NAME_HELP.unit}
        error={errorFor(["name"])}
      />
      {renamed && (
        <p className="t-caption">
          Onboarding pages are looked up under a unit&apos;s name, so the seats in it read the pages
          under the new name.
        </p>
      )}
      <ConfigField
        label="Key"
        kind="id"
        value={form.key}
        onChange={(value) => set({ key: value })}
        disabled={disabled}
        help={saved ? ADDRESS_HELP.unit.saved : ADDRESS_HELP.unit.created}
        error={errorFor(["key"])}
      />
      <UnitTypeField
        value={form.type}
        onChange={(type) => set({ type })}
        error={errorFor(["type"])}
        disabled={disabled}
      />
      <ConfigField
        label="Purpose"
        kind="multiline"
        value={form.purpose}
        onChange={(purpose) => set({ purpose })}
        required={false}
        disabled={disabled}
        error={errorFor(["purpose"])}
      />
      <ListInput
        label="Goals"
        itemName="goal"
        multiline
        value={form.goals}
        onChange={(goals) => set({ goals })}
        optional
        disabled={disabled}
        error={withProblems(errorFor(["goals"]))}
      />

      <EditorSection title="Leadership">
        <ConfigField
          label="Lead"
          kind="choice"
          choices={leadChoices}
          value={form.lead}
          onChange={(lead) => set({ lead })}
          disabled={disabled}
          autoFocus={section === "leadership"}
          help="The lead manages the unit's direct members, and a unit below that names no lead inherits this one."
          error={errorFor(["lead"])}
        />
        <ConfigField
          label="Channel"
          value={form.channel}
          onChange={(channel) => set({ channel })}
          required={false}
          disabled={disabled}
          placeholder={inheritedChannel}
          help={
            inheritedChannel && parent
              ? `Empty inherits ${inheritedChannel} from ${parent.data.name}.`
              : "A unit below that names no channel inherits this one."
          }
          error={errorFor(["channel"])}
        />
      </EditorSection>

      <ListInput
        label="Knowledge"
        itemName="reference"
        value={form.knowledge}
        onChange={(knowledge) => set({ knowledge })}
        optional
        disabled={disabled}
        helper="Free-text references, not a read scope."
        error={withProblems(errorFor(["knowledge_refs"]))}
      />

      <Owns
        who="unit"
        project={form.project}
        space={form.space}
        onProject={(project) => set({ project })}
        onSpace={(space) => set({ space })}
        errorFor={errorFor}
        disabled={disabled}
      />

      {runtimeVisible ? (
        <>
          <ScheduleToggles
            data={unit.data}
            unit
            values={form.schedules}
            onChange={(name, enabled) => set({ schedules: { ...form.schedules, [name]: enabled } })}
            disabled={disabled}
            error={errorFor(["runtime", "schedules"])}
          />
          {toolCredentialNames(unit.data).length > 0 && (
            <EditorSection title="In the runtime half">
              <ToolCredentialFact data={unit.data} />
            </EditorSection>
          )}
        </>
      ) : (
        <RuntimeHidden what="unit" />
      )}
    </EditorShell>
  );
}

// ---------------------------------------------------------------------------
// A seat
// ---------------------------------------------------------------------------

const GITHUB_TIER: Segment[] = ["runtime", "github", "tier"];
const GITHUB_REPOS: Segment[] = ["runtime", "github", "repos"];
const MATTERMOST_CHANNEL: Segment[] = ["runtime", "mattermost", "channel"];
const MATTERMOST_USERNAME: Segment[] = ["runtime", "mattermost", "username"];

/** A seat of the draft by handle, as a person reads it: its name, or its handle. */
function seatLabel(draft: Parameters<typeof allSeats>[0], handle: string): string {
  for (const { seat } of allSeats(draft)) {
    if (seat.data.handle === handle) return seat.data.name || `@${handle}`;
  }
  return `@${handle}`;
}

/**
 * Every seat of the draft as a choice, by handle: its name, with the handle
 * beside it where another seat shares the name, so two seats called the same
 * are two choices a reader can tell apart.
 */
function seatChoices(draft: Parameters<typeof allSeats>[0], except?: NodeKey): FieldChoice[] {
  const seats = [...allSeats(draft)].map(({ seat }) => seat).filter((s) => s.key !== except);
  const count = new Map<string, number>();
  for (const s of seats) count.set(s.data.name, (count.get(s.data.name) ?? 0) + 1);
  return seats.map((s) => ({
    value: s.data.handle,
    label:
      s.data.name === ""
        ? `@${s.data.handle}`
        : count.get(s.data.name)! > 1
          ? `${s.data.name} (@${s.data.handle})`
          : s.data.name,
  }));
}

/** The field paths a seat's form draws; see [unitFieldPaths] for why it matters. */
function seatFieldPaths(
  company: CompanyDocument,
  data: SeatData,
  { human, runtimeVisible }: { human: boolean; runtimeVisible: boolean },
): Segment[][] {
  const runtime: Segment[][] = !runtimeVisible
    ? []
    : human
      ? [
          ["runtime", "contact"],
          ["runtime", "availability"],
        ]
      : [
          ["runtime", "llm"],
          ["runtime", "token_budget"],
          ...(schedulesOf(data).length > 0 ? [["runtime", "schedules"] as Segment[]] : []),
          ...(isConnected(company, "github") ? [GITHUB_TIER, GITHUB_REPOS] : []),
          ...(isConnected(company, "mattermost") &&
          isRecord(getPath(data, ["runtime", "mattermost"]))
            ? [MATTERMOST_CHANNEL, MATTERMOST_USERNAME]
            : []),
        ];
  return [
    ["name"],
    ["handle"],
    ["email"],
    ["goal"],
    ["backstory"],
    ["responsibilities"],
    ["manages"],
    ["project"],
    ["space"],
    ...(human ? [] : [["behavioral_guidelines"] as Segment[]]),
    ...runtime,
  ];
}

/**
 * A seat's address, which the chart SEALS LIKE A CREDENTIAL and serves only as
 * the `${VAR}` reference it was sealed under or, where a row holds something
 * else, the mask — never a value a person could read as the address.
 *
 * EITHER READS AS "SET, SEALED", never as text in a box. Shown in one,
 * `__redacted__` looked like an address somebody had typed, and a person
 * clearing the box to type over it had cleared the address before they knew
 * it; and the reference is a name the CHART minted for its own secret store
 * entry, so a box holding it invited an edit that would point the seat at an
 * entry nobody stored. The reference is still shown, as the name it is, so a
 * person can tell a sealed address from a hidden one. An untouched field sends
 * back exactly what was read — the chart keeps a reference it already holds
 * and restores a mask from the row — so nothing about it is a change until
 * somebody chooses to replace it.
 */
function AddressField({
  initial,
  value,
  onChange,
  disabled,
  error,
}: {
  initial: string;
  value: string;
  onChange: (value: string) => void;
  disabled: boolean;
  error: string | undefined;
}) {
  const reference = isWholeReference(initial);
  const hidden = initial === REDACTED || reference;
  if (hidden && value === initial) {
    return (
      <ReadOnlyFact
        label="Email"
        reason="The chart keeps an address sealed and never sends it to a browser. Saving leaves it as it is."
      >
        <span className="row gap-2">
          {reference ? (
            <span>
              An address is set, sealed as <InlineCode>{initial.trim()}</InlineCode>.
            </span>
          ) : (
            <span>An address is set (hidden).</span>
          )}
          <Button variant="secondary" size="small" disabled={disabled} onClick={() => onChange("")}>
            Replace
          </Button>
        </span>
      </ReadOnlyFact>
    );
  }
  return (
    <div className="col gap-1">
      <ConfigField
        label="Email"
        kind="email"
        value={value}
        onChange={onChange}
        required={false}
        disabled={disabled}
        help={hidden ? "Replaces the sealed address. Left empty, it removes it." : undefined}
        error={error}
      />
      {hidden && (
        <Button
          variant="tertiary"
          size="small"
          disabled={disabled}
          onClick={() => onChange(initial)}
          style={{ alignSelf: "flex-start" }}
        >
          Keep the sealed address
        </Button>
      )}
    </div>
  );
}

const GITHUB_TIERS: FieldChoice[] = [
  { value: "", label: "Not set (read only)" },
  { value: "read_only", label: "Read only" },
  { value: "review", label: "Review" },
  { value: "full_access", label: "Full access" },
];

function SeatEditor({
  seat,
  section,
  onClose,
}: {
  seat: DraftSeat;
  section: EditorSectionName | undefined;
  onClose: () => void;
}) {
  const api = useBuilder();
  const { state } = api;
  const key = seat.key;
  const data = seat.data;
  const company = state.draft.company;
  const minted = isMintedKey(key);
  const human = kindOf(data) === "human";
  const handle = data.handle;
  const runtimeVisible = state.base.runtimeVisible;
  const { initial, form, set, dirty } = useForm<SeatForm>(() =>
    seatForm(data, gitLabAccessLevel(company, handle)),
  );
  const { refusal, apply } = useApply(api, key, onClose);
  const { errorFor, rest } = placeOnFields(
    placedOn(api, key),
    seatFieldPaths(company, data, { human, runtimeVisible }),
  );
  const disabled = api.readOnly;

  const budgetError = human || !runtimeVisible ? undefined : tokenBudgetError(form.tokenBudget);
  const blocked =
    form.name.trim() === ""
      ? "A seat needs a name."
      : form.handle.trim() === ""
        ? "A seat needs a handle."
        : budgetError
          ? "Correct the token budget first."
          : null;

  return (
    <EditorShell
      title={`Edit ${data.name || "seat"}`}
      mark={human ? "human" : "agent"}
      name={data.name || "this seat"}
      dirty={dirty}
      blocked={blocked}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      onApply={() => apply(seatParts(key, initial, form))}
      onClose={onClose}
    >
      <ConfigField
        label="Name"
        value={form.name}
        onChange={(name) => set({ name })}
        disabled={disabled}
        help={NAME_HELP.seat}
        error={errorFor(["name"])}
      />
      <ConfigField
        label="Handle"
        kind="id"
        value={form.handle}
        onChange={(value) => set({ handle: value })}
        disabled={disabled}
        help={minted ? ADDRESS_HELP.seat.created : ADDRESS_HELP.seat.saved}
        error={errorFor(["handle"])}
      />
      <KindFact seatKey={key} human={human} dirty={dirty} onClose={onClose} />
      <AddressField
        initial={initial.email}
        value={form.email}
        onChange={(email) => set({ email })}
        disabled={disabled}
        error={errorFor(["email"])}
      />
      <ConfigField
        label="Goal"
        kind="multiline"
        value={form.goal}
        onChange={(goal) => set({ goal })}
        required={false}
        disabled={disabled}
        error={errorFor(["goal"])}
      />
      <ConfigField
        label="Backstory"
        kind="multiline"
        value={form.backstory}
        onChange={(backstory) => set({ backstory })}
        required={false}
        disabled={disabled}
        error={errorFor(["backstory"])}
      />
      <ListInput
        label="Responsibilities"
        itemName="responsibility"
        multiline
        value={form.responsibilities}
        onChange={(responsibilities) => set({ responsibilities })}
        optional
        disabled={disabled}
        error={withProblems(errorFor(["responsibilities"]))}
      />
      {!human && (
        <ListInput
          label="Behavioral guidelines"
          itemName="guideline"
          multiline
          value={form.guidelines}
          onChange={(guidelines) => set({ guidelines })}
          optional
          disabled={disabled}
          error={withProblems(errorFor(["behavioral_guidelines"]))}
        />
      )}

      <Reports
        seat={seat}
        value={form.manages}
        onChange={(manages) => set({ manages })}
        disabled={disabled}
        error={errorFor(["manages"])}
        autoFocus={section === "reports"}
      />

      <Owns
        who="seat"
        project={form.project}
        space={form.space}
        onProject={(project) => set({ project })}
        onSpace={(space) => set({ space })}
        errorFor={errorFor}
        disabled={disabled}
      />

      {!runtimeVisible ? (
        <RuntimeHidden what="seat" />
      ) : human ? (
        <EditorSection
          title="Contact"
          hint="Optional: how agents @-mention and reach the person on each surface. A person who works only through the dashboard needs none, and the chart check names the seat until one is added."
        >
          {CONTACT_IDENTITIES.map(({ key: identity, label }) => (
            <ConfigField
              key={identity}
              label={label}
              kind="id"
              value={form.contact[identity]}
              onChange={(value) => set({ contact: { ...form.contact, [identity]: value } })}
              required={false}
              disabled={disabled}
            />
          ))}
          {errorFor(["runtime", "contact"]) && (
            <Callout variant="danger" role="alert">
              {errorFor(["runtime", "contact"])}
            </Callout>
          )}
          <ConfigField
            label="Availability"
            value={form.availability}
            onChange={(availability) => set({ availability })}
            required={false}
            disabled={disabled}
            help="A free-text note, such as working hours."
            error={errorFor(["runtime", "availability"])}
          />
        </EditorSection>
      ) : (
        <>
          <ModelSection
            chain={form.llm}
            onChain={(llm) => set({ llm })}
            budget={form.tokenBudget}
            onBudget={(tokenBudget) => set({ tokenBudget })}
            budgetError={budgetError ?? errorFor(["runtime", "token_budget"])}
            chainError={errorFor(["runtime", "llm"])}
            disabled={disabled}
          />
          <ScheduleToggles
            data={data}
            unit={false}
            values={form.schedules}
            onChange={(name, enabled) => set({ schedules: { ...form.schedules, [name]: enabled } })}
            disabled={disabled}
            error={errorFor(["runtime", "schedules"])}
          />
          <IntegrationsSection
            seatKey={key}
            data={data}
            initial={initial}
            form={form}
            set={set}
            errorFor={errorFor}
            disabled={disabled}
          />
          <RuntimeFacts data={data} handle={handle} />
          {/*
           * Last, where the console's agent editor ends too. An agent seat is
           * the only node the chart tints, so it is the only one with a hue
           * to state: a human seat wears the dashed boundary and a unit is
           * the chart's own neutral surface (`nodeTone.ts`).
           */}
          <HueFact nodeKey={key} />
        </>
      )}
    </EditorShell>
  );
}

/** A seat's kind, and the door to changing it, which is its own operation. */
function KindFact({
  seatKey,
  human,
  dirty,
  onClose,
}: {
  seatKey: NodeKey;
  human: boolean;
  dirty: boolean;
  onClose: () => void;
}) {
  const api = useBuilder();
  return (
    <ReadOnlyFact
      label="Kind"
      reason={
        dirty
          ? "Changing the kind removes fields, so it is its own step. Apply or discard the changes in this form first."
          : "Changing the kind removes the fields the other kind may not carry, so it is its own step."
      }
    >
      <div className="row wrap">
        <span>{human ? "Human seat" : "Agent seat"}</span>
        <Button
          variant="secondary"
          size="small"
          disabled={dirty || api.readOnly}
          onClick={() => {
            onClose();
            api.openChangeKind(seatKey);
          }}
        >
          {human ? "Change to agent seat" : "Change to human seat"}
        </Button>
      </div>
    </ReadOnlyFact>
  );
}

/**
 * Whom the seat manages: the explicit list, edited, and the seats it manages
 * automatically as a unit lead, shown apart because removing one from the
 * list would change nothing (the engine adds it back).
 */
function Reports({
  seat,
  value,
  onChange,
  disabled,
  error,
  autoFocus,
}: {
  seat: DraftSeat;
  value: readonly string[];
  onChange: (next: string[]) => void;
  disabled: boolean;
  error?: string;
  /** Starts the form here: the editor was opened at the seat's reports. */
  autoFocus: boolean;
}) {
  const { state } = useBuilder();
  // BY ADDRESS, as the chart stores an entry: a seat's handle or a unit's
  // key. A handle and a key never collide in one draft that the engine would
  // take — an entry resolves to the seat first — so a unit whose key a seat's
  // handle already is, is not offered as a second meaning of it.
  const options: TagsInputOption[] = seatChoices(state.draft, seat.key).map((choice) => ({
    value: choice.value,
    label: choice.label,
    group: "Seats",
  }));
  const handles = new Set([...allSeats(state.draft)].map(({ seat: s }) => s.data.handle));
  for (const { unit } of allUnits(state.draft)) {
    if (handles.has(unit.data.key)) continue;
    options.push({
      value: unit.data.key,
      label: unit.data.name || unit.data.key,
      group: "Units",
      description: "every seat in it",
    });
  }

  // WHO IT MANAGES AS A LEAD is the engine's derivation, of the SAVED chart:
  // shown while the draft's chart is still that one, and said to be derived
  // on saving otherwise.
  const saved = savedDerivation(state);
  const derived = derivedSeatOf(state, seat.key);
  const automatic = new Set(derived?.auto_reports ?? []);
  const groups = (saved?.derived.units ?? [])
    .filter((u) => derived?.handle && u.lead === derived.handle)
    .map((u) => ({
      unit: u.name,
      names: (u.seats ?? []).filter((h) => automatic.has(h)).map((h) => nameOfHandle(state, h)),
    }))
    .filter((g) => g.names.length > 0);

  return (
    <EditorSection title="Reports">
      <FormField
        label="Manages"
        optional
        helper="The seats this seat manages, or a unit to manage every seat in it."
        error={withProblems(error)}
      >
        {(field) => (
          <TagsInput
            id={field.id}
            label="Managed seats and units"
            value={[...value]}
            onChange={onChange}
            options={options}
            // ONLY WHAT THE COMPANY HAS. A seat manages a seat or a unit that
            // exists; a typed name that matches neither is a dangling
            // reference the engine reports rather than a value to accept here.
            allowCustom={false}
            disabled={disabled}
            focusOnMount={autoFocus}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
          />
        )}
      </FormField>
      {groups.map((g) => (
        <ReadOnlyFact
          key={g.unit}
          label={`Managed automatically as lead of ${g.unit}`}
          reason="A unit lead manages the unit's direct members unless another member manages them. Change the unit's lead to change this."
        >
          {g.names.join(", ")}
        </ReadOnlyFact>
      ))}
      {!saved && state.base.derived !== null && (
        <p className="builder-note muted">
          Who this seat manages as a unit lead is derived by the engine once this draft is saved.
        </p>
      )}
    </EditorSection>
  );
}

function ModelSection({
  chain,
  onChain,
  budget,
  onBudget,
  budgetError,
  chainError,
  disabled,
}: {
  chain: readonly string[];
  onChain: (next: string[]) => void;
  budget: string;
  onBudget: (next: string) => void;
  budgetError: string | undefined;
  chainError: string | undefined;
  disabled: boolean;
}) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const providers = providerOrder(company);
  const unpinned = unpinnedProvider(company);
  return (
    <EditorSection title="Model">
      <FormField
        label="Model"
        optional
        helper={
          chain.length > 0
            ? `Tried in this order: ${chain.join(", then ")}.`
            : unpinned
              ? `Runs on ${unpinned}, the provider a seat that names none runs on.`
              : "The company has no model provider yet. Add one in the settings."
        }
        error={withProblems(chainError)}
      >
        {(field) => (
          <TagsInput
            id={field.id}
            label="Model providers"
            value={[...chain]}
            onChange={onChain}
            options={providers.map((key) => ({ value: key, label: key }))}
            allowCustom={false}
            // THE ORDER IS THE FALLBACK CHAIN, read first to last, so it is
            // moved rather than retyped. The help line used to end "to change
            // the order, remove a provider and choose it again", which was a
            // workaround for a control that could not reorder.
            ordered
            disabled={disabled}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
          />
        )}
      </FormField>
      <ConfigField
        label="Token budget"
        kind="id"
        value={budget}
        onChange={onBudget}
        required={false}
        disabled={disabled}
        help="Tokens this seat may spend. Empty or 0 is unlimited."
        error={budgetError}
      />
    </EditorSection>
  );
}

function IntegrationsSection({
  seatKey,
  data,
  initial,
  form,
  set,
  errorFor,
  disabled,
}: {
  seatKey: NodeKey;
  data: SeatData;
  initial: SeatForm;
  form: SeatForm;
  set: (patch: Partial<SeatForm>) => void;
  errorFor: (path: readonly Segment[]) => string | undefined;
  disabled: boolean;
}) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const minted = isMintedKey(seatKey);
  const github = getPath(data, ["runtime", "github"]);
  const appSlug = isRecord(github) && typeof github.app_slug === "string" ? github.app_slug : "";
  const enrolling = !isRecord(github) && (form.githubTier !== "" || form.githubRepos.length > 0);
  const tierChanged = appSlug !== "" && form.githubTier !== initial.githubTier;
  const mattermost = getPath(data, ["runtime", "mattermost"]);
  // A BOT IS THE ENGINE'S ONLY WHERE ITS TOKEN NAMES A SECRET STORE ENTRY.
  // The provisioner mints into the entry a whole `${VAR}` points at and skips
  // every other seat with a note (`mattermost.PlanFor`), so a literal token,
  // which reaches this screen as its mask, is a bot somebody manages by hand
  // and whose username is theirs to correct.
  const provisioned = isRecord(mattermost) && isWholeReference(mattermost.bot_token);
  // THE PROVISIONER NAMES A BOT AFTER THE HANDLE THE SEAT WAS CREATED UNDER
  // (`provision.Origin`), which no rename moves: a saved seat's KEY, which is
  // its identity (`keys.ts`), and a seat this draft creates, the handle it
  // will be created under.
  const origin = minted ? form.handle.trim() : (handleOfKey(seatKey) ?? "");
  const defaultUsername = origin === "" ? "" : mattermostBotUsername(company, origin);
  const gitlabDefault = defaultGitLabAccessLevel(company);

  return (
    <EditorSection title="Integrations">
      <EditorSection title="GitHub">
        {isConnected(company, "github") ? (
          <>
            {appSlug && (
              <ReadOnlyFact
                label="App"
                reason="Created from Integrations; its credentials are never shown."
              >
                <InlineCode>{appSlug}</InlineCode>
              </ReadOnlyFact>
            )}
            <ConfigField
              label="Access tier"
              kind="choice"
              choices={GITHUB_TIERS}
              value={form.githubTier}
              onChange={(githubTier) => set({ githubTier })}
              disabled={disabled}
              error={errorFor(GITHUB_TIER)}
            />
            <ListInput
              label="Repositories"
              itemName="repository"
              value={form.githubRepos}
              onChange={(githubRepos) => set({ githubRepos })}
              optional
              disabled={disabled}
              placeholder="owner/name"
              helper="Empty means every repository the installation covers."
              error={withProblems(errorFor(GITHUB_REPOS))}
            />
            {enrolling && (
              <Callout variant="info">
                This enrols the seat in GitHub. Create its app from Integrations.{" "}
                <ScreenLink to="integrations">Open Integrations</ScreenLink>
              </Callout>
            )}
            {tierChanged && (
              <Callout variant="warning">
                The app's permissions were fixed when it was created. Raise them at GitHub as well.
              </Callout>
            )}
          </>
        ) : (
          <NotConnected tool="github" />
        )}
      </EditorSection>

      <EditorSection title="Mattermost">
        {!isConnected(company, "mattermost") ? (
          <NotConnected tool="mattermost" />
        ) : isRecord(mattermost) ? (
          <>
            <ConfigField
              label="Mattermost channel"
              value={form.mattermostChannel}
              onChange={(mattermostChannel) => set({ mattermostChannel })}
              required={false}
              disabled={disabled}
              help="The name of this seat's default channel."
              error={errorFor(MATTERMOST_CHANNEL)}
            />
            {provisioned ? (
              <ReadOnlyFact
                label="Bot username"
                reason="The engine provisions this bot, because its token names a secret store entry. Changing the username would make the provisioner find or create a second bot."
              >
                {form.mattermostUsername || defaultUsername ? (
                  <InlineCode>{form.mattermostUsername || defaultUsername}</InlineCode>
                ) : (
                  "The name the provisioner gives the handle this seat was created under"
                )}
              </ReadOnlyFact>
            ) : (
              <ConfigField
                label="Bot username"
                kind="id"
                value={form.mattermostUsername}
                onChange={(mattermostUsername) => set({ mattermostUsername })}
                required={false}
                disabled={disabled}
                help={
                  defaultUsername
                    ? `The engine provisions a bot only where its token names a secret store entry, so this one is managed by hand. Empty uses ${defaultUsername}.`
                    : "The engine provisions a bot only where its token names a secret store entry, so this one is managed by hand. Empty uses the name the provisioner gives the handle this seat was created under."
                }
                error={errorFor(MATTERMOST_USERNAME)}
              />
            )}
          </>
        ) : (
          <p className="builder-note muted">
            This seat has no Mattermost bot of its own, so it has no channel to set.{" "}
            <ScreenLink to="integrations">Open Integrations</ScreenLink>
          </p>
        )}
      </EditorSection>

      <EditorSection title="GitLab">
        {!isConnected(company, "gitlab") ? (
          <NotConnected tool="gitlab" />
        ) : !hasGitLabProvisioning(company) ? (
          <p className="builder-note muted">
            GitLab provisioning is not set up, so there is no access level to set.{" "}
            <ScreenLink to="integrations">Open Integrations</ScreenLink>
          </p>
        ) : (
          <ConfigField
            label="Access level"
            kind="choice"
            choices={[
              { value: "", label: `Company default (${gitlabDefault})` },
              { value: "developer", label: "Developer" },
              { value: "maintainer", label: "Maintainer" },
            ]}
            value={form.accessLevel}
            onChange={(accessLevel) => set({ accessLevel })}
            disabled={disabled}
            help="The level this seat's GitLab account joins the group with. Kept by handle, so a new handle carries it."
          />
        )}
      </EditorSection>
    </EditorSection>
  );
}

/** The seat's runtime settings the builder shows and does not edit, each with why. */
function RuntimeFacts({ data, handle }: { data: SeatData; handle: string }) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const facts: ReactNode[] = [];
  const runtime = isRecord(data.runtime) ? data.runtime : {};
  const chainOf = (value: unknown) => providerKeys(value).join(", then ");

  const phases = PHASE_MODEL_FIELDS.filter((field) => chainOf(runtime[field]) !== "");
  if (phases.length > 0) {
    facts.push(
      <ReadOnlyFact key="phases" label="Models per phase" reason={RUNTIME_ELSEWHERE}>
        <ul className="builder-list">
          {phases.map((field) => (
            <li key={field}>
              {field.replace("llm_", "")}: {chainOf(runtime[field])}
            </li>
          ))}
        </ul>
      </ReadOnlyFact>,
    );
  }
  if (isRecord(runtime.sandbox)) {
    const enabled = runtime.sandbox.enabled === true ? "Enabled" : "Not enabled";
    const runIn =
      typeof runtime.sandbox.run_in === "string" ? `, runs in ${runtime.sandbox.run_in}` : "";
    facts.push(
      <ReadOnlyFact
        key="sandbox"
        label="Sandbox"
        reason={`A sandbox runs on the settings' providers.sandbox block. ${RUNTIME_ELSEWHERE}`}
      >
        {enabled}
        {runIn}
      </ReadOnlyFact>,
    );
  }
  if (Array.isArray(runtime.workers) && runtime.workers.length > 0) {
    facts.push(
      <ReadOnlyFact
        key="workers"
        label="Workers"
        reason={`Workers name templates from the settings' workers block. ${RUNTIME_ELSEWHERE}`}
      >
        {runtime.workers.join(", ")}
      </ReadOnlyFact>,
    );
  }
  if (isRecord(runtime.placement)) {
    facts.push(
      <ReadOnlyFact
        key="placement"
        label="Placement"
        reason={`Which nodes run this seat, a fleet setting. ${RUNTIME_ELSEWHERE}`}
      >
        {placementSummary(runtime.placement)}
      </ReadOnlyFact>,
    );
  }
  if (typeof runtime.learning_enabled === "boolean") {
    facts.push(
      <ReadOnlyFact key="learning" label="Learning" reason={RUNTIME_ELSEWHERE}>
        {runtime.learning_enabled ? "On" : "Off"}
      </ReadOnlyFact>,
    );
  }
  if (toolCredentialNames(data).length > 0)
    facts.push(<ToolCredentialFact key="mcp" data={data} />);
  if (isConnected(company, "datadog")) {
    // A block that is switched off wakes nobody, whatever its route_to says,
    // which is how the engine reads it and how the chart draws it.
    const on = datadogEnabled(company);
    const fallback = datadogFallback(company) === handle;
    facts.push(
      <ReadOnlyFact
        key="datadog"
        label="Datadog fallback"
        reason="An alert whose tags name no seat wakes the fallback seat. It is chosen from Integrations, or here when the fallback seat is deleted or changed to a human seat."
        link={{ to: "integrations", label: "Open Integrations" }}
      >
        {!on
          ? "Datadog is switched off, so no alert wakes a fallback seat."
          : fallback
            ? "This seat is the Datadog fallback."
            : "Not the Datadog fallback."}
      </ReadOnlyFact>,
    );
  }
  if (facts.length === 0) return null;
  return (
    <EditorSection
      title="Shown, not edited here"
      hint={`${plural(facts.length, "setting")} the builder shows and does not edit.`}
    >
      {facts}
    </EditorSection>
  );
}
