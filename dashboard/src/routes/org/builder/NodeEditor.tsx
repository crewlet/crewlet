/**
 * The node editor: the company's charter, a unit, or a seat, in a side sheet.
 *
 * FORM-LOCAL, APPLIED AS ONE OPERATION. Every field is held in a local form
 * until Apply, which records the fields that changed as one `edit` (see
 * `editorForm.ts`): nothing reaches the draft while a sentence is half
 * typed, a refusal keeps the form open with the engine's reason on screen,
 * and Undo takes back the whole of what Apply did. Closing a form with
 * changes in it, by Cancel, Close, Escape or the veil, asks first, because a
 * form's changes exist nowhere else.
 *
 * WHAT IS EDITABLE IS DECIDED FIELD BY FIELD (the field coverage of the
 * builder's spec). A field the builder can write is a field. A field it shows
 * but cannot write is a read-only fact that says why and where it is edited
 * instead, because each needs a company-level block the builder does not
 * edit, or because changing it here would change something it must not (an
 * existing seat's handle is its identity). Fields that are neither are not
 * drawn: the configuration document keeps them, and the builder preserves
 * every key it does not model.
 *
 * A FIELD FOR A TOOL IS DRAWN ONLY WHERE THE TOOL IS. A seat's GitHub tier,
 * Slack channel, Mattermost channel, GitLab access level, Jira project and
 * Confluence space mean something only when the company has connected that
 * tool, so each says "<Tool> is not connected" instead of offering a setting
 * that does nothing. Two of them change more than their field, and say so
 * beside it: a GitHub tier on a seat with no GitHub block enrols the seat,
 * and a tier change on a seat whose app exists does not reach the app, whose
 * permissions were fixed when it was created.
 *
 * NO CREDENTIAL IS RENDERED. Tool credentials appear as server and variable
 * names, a seat's chat app blocks as the settings a person writes (a channel,
 * a username), and nothing else of theirs.
 *
 * THE ENGINE'S PROBLEMS SIT BESIDE THE FIELDS THEY NAME, placed through the
 * same path index the check's answer was placed with; whatever names no
 * drawn field is listed at the top, so nothing the engine said is dropped.
 */

import { useState, type ReactNode } from "react";
import type { CompanyDocument, ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { formatPhaseLLM, plural } from "~/lib/format.ts";
import { Checkbox } from "~/ui/Checkbox.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { Drawer } from "~/ui/Drawer.tsx";
import { Field, type FieldChoice } from "~/ui/Field.tsx";
import { ListField } from "~/ui/ListField.tsx";
import { MultiPicker, type PickerOption } from "~/ui/MultiPicker.tsx";
import { Banner, Button, Empty } from "~/ui/primitives.tsx";
import { useBuilder, type BuilderApi } from "./BuilderContext.tsx";
import {
  companyForm,
  companyParts,
  editIntent,
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
  EditorSection,
  NodeProblems,
  NotConnected,
  ReadOnlyFact,
  Refusal,
  ScreenLink,
  UnitTypeField,
  placeOnFields,
} from "./dialogParts.tsx";
import type { Segment } from "./model/document.ts";
import { allSeats, allUnits, locate, type DraftSeat, type DraftUnit } from "./model/draft.ts";
import { getPath, isRecord, jsonEqual } from "./model/json.ts";
import { COMPANY_KEY, isMintedKey, type NodeKey } from "./model/keys.ts";
import { kindOf, type EditPartIntent } from "./model/operations.ts";
import type { PlacedProblem } from "./model/problems.ts";
import { recordIntent } from "./model/reducer.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import {
  PHASE_MODEL_FIELDS,
  datadogFallback,
  defaultGitLabAccessLevel,
  derivedSeatOf,
  derivedUnitOf,
  gitLabAccessLevel,
  handleOf,
  hasGitLabProvisioning,
  isConnected,
  nameOfHandle,
  providerOrder,
  toolCredentialNames,
  unpinnedProvider,
} from "./nodeFacts.ts";
import { RenameUnitPreflight } from "./RenameUnitPreflight.tsx";

export function NodeEditor({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const { state } = useBuilder();
  if (nodeKey === COMPANY_KEY) return <CompanyEditor onClose={onClose} />;
  const found = locate(state.draft, nodeKey);
  if (!found) {
    return (
      <Drawer title="Edit" onClose={onClose}>
        <Empty
          title="This node is no longer in the draft"
          hint="It was removed by an undo or an update onto a newer revision. Close this editor and choose another node."
        />
      </Drawer>
    );
  }
  // KEYED BY THE NODE, so opening the editor on another node builds a new
  // form from that node rather than keeping the one the last node filled: a
  // form's fields are state, and React would otherwise reuse them.
  return found.kind === "unit" ? (
    <UnitEditor key={found.node.key} unit={found.node} onClose={onClose} />
  ) : (
    <SeatEditor key={found.node.key} seat={found.node} onClose={onClose} />
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

function EditorShell({
  title,
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
  const [confirming, setConfirming] = useState(false);
  const requestClose = () => (dirty ? setConfirming(true) : onClose());
  const disabled = readOnly || blocked !== null;
  return (
    <>
      <Drawer
        title={title}
        icon="pencil"
        onClose={requestClose}
        // ALWAYS A FORM. The panel is a <form> only while it has a submit
        // handler, and swapping the element as Apply became unavailable would
        // remount every field in it, dropping focus and a list's edit in
        // progress. The guard lives in the handler instead.
        onSubmit={() => {
          if (!disabled) onApply();
        }}
        footer={
          <>
            {blocked && !readOnly && <span className="t-caption spacer">{blocked}</span>}
            <Button variant="ghost" onClick={requestClose}>
              Cancel
            </Button>
            <Button variant="primary" type="submit" disabled={disabled}>
              Apply
            </Button>
          </>
        }
      >
        {readOnly && (
          <Banner tone="neutral">
            The organization cannot be changed right now, so these fields are for reading.
          </Banner>
        )}
        <Refusal message={refusal} />
        <NodeProblems problems={problems} />
        {children}
      </Drawer>
      {confirming && (
        <Dialog
          title="Discard your changes?"
          icon="alert"
          onClose={() => setConfirming(false)}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirming(false)}>
                Keep editing
              </Button>
              <Button variant="danger" onClick={onClose}>
                Discard changes
              </Button>
            </>
          }
        >
          <p className="t-body">
            The changes to {name} have not been applied to the draft. Discarding them leaves the
            draft as it was.
          </p>
        </Dialog>
      )}
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
  data: ConfigRole | ConfigUnit;
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
          Schedules are written in the configuration document. Here each can be switched on or off.{" "}
          <ScreenLink to={["schedules"]}>Open Schedules</ScreenLink>
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
            onChange={(checked) => onChange(s.name, checked)}
            disabled={disabled}
          />
        );
      })}
      {error && <Banner tone="critical">{error}</Banner>}
    </EditorSection>
  );
}

/** Tool credentials as names, never values. */
function ToolCredentialFact({ data }: { data: ConfigRole | ConfigUnit }) {
  const names = toolCredentialNames(data);
  if (names.length === 0) return null;
  return (
    <ReadOnlyFact
      label="Tool credentials"
      reason="They name servers the company's mcp_servers block defines, so they are set in the configuration document. Values are never shown."
      link={{ to: ["config"], label: "Open the configuration" }}
    >
      <ul className="builder-list">
        {names.map(({ server, variables }) => (
          <li key={server}>
            <code className="inline">{server}</code>
            {variables.length > 0 && ` (${variables.join(", ")})`}
          </li>
        ))}
      </ul>
    </ReadOnlyFact>
  );
}

const JIRA_PROJECT: Segment[] = ["integrations", "jira", "project"];
const CONFLUENCE_SPACE: Segment[] = ["integrations", "confluence", "space"];

/** Jira project and Confluence space: ownership for routing, not a permission. */
function Owns({
  who,
  jira,
  confluence,
  onJira,
  onConfluence,
  errorFor,
  disabled,
}: {
  who: "seat" | "unit";
  jira: string;
  confluence: string;
  onJira: (next: string) => void;
  onConfluence: (next: string) => void;
  errorFor: (path: readonly Segment[]) => string | undefined;
  disabled: boolean;
}) {
  const { state } = useBuilder();
  const company = state.draft.company;
  return (
    <EditorSection
      title="Owns"
      hint={`Where unrouted work for this ${who} goes. Not a permission.`}
    >
      {isConnected(company, "jira") ? (
        <Field
          label="Jira project"
          kind="id"
          value={jira}
          onChange={onJira}
          required={false}
          disabled={disabled}
          error={errorFor(JIRA_PROJECT)}
        />
      ) : (
        <NotConnected tool="jira" />
      )}
      {isConnected(company, "confluence") ? (
        <Field
          label="Confluence space"
          kind="id"
          value={confluence}
          onChange={onConfluence}
          required={false}
          disabled={disabled}
          error={errorFor(CONFLUENCE_SPACE)}
        />
      ) : (
        <NotConnected tool="confluence" />
      )}
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

  const savedName = typeof state.base.document?.name === "string" ? state.base.document.name : "";
  const renaming = savedName !== "" && form.name.trim() !== savedName;
  const blocked =
    form.name.trim() === ""
      ? "The company needs a name."
      : renaming && !acknowledged
        ? "Confirm what renaming the company does."
        : null;

  return (
    <EditorShell
      title="Edit the charter"
      name="the charter"
      dirty={dirty}
      blocked={blocked}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      onApply={() => apply(companyParts(initial, form))}
      onClose={onClose}
    >
      <Field
        label="Company name"
        value={form.name}
        onChange={(name) => set({ name })}
        error={errorFor(["name"])}
      />
      {renaming && (
        <Checkbox
          framed
          tone="critical"
          label="I understand what renaming the company does"
          description="An agent seat's id is derived from the company name and its handle, so every agent seat gets a new id: each seat's diary and onboarding progress stay under the old id and are no longer read, and every agent seat onboards again. Handles, mailboxes and episodes are unchanged."
          checked={acknowledged}
          onChange={setAcknowledged}
        />
      )}
      <Field
        label="Mission"
        kind="multiline"
        value={form.mission}
        onChange={(mission) => set({ mission })}
        required={false}
        error={errorFor(["mission"])}
      />
      <Field
        label="Vision"
        kind="multiline"
        value={form.vision}
        onChange={(vision) => set({ vision })}
        required={false}
        error={errorFor(["vision"])}
      />
      <ListField
        label="Policies"
        itemName="policy"
        multiline
        value={form.policies}
        onChange={(policies) => set({ policies })}
        required={false}
        error={errorFor(["policies"])}
      />
    </EditorShell>
  );
}

// ---------------------------------------------------------------------------
// A unit
// ---------------------------------------------------------------------------

/**
 * The field paths a unit's form draws, so a problem the engine reported on a
 * field this form does NOT draw (a Jira project on a company that has not
 * connected Jira) is listed at the top rather than attached to a field nobody
 * can see.
 */
function unitFieldPaths(company: CompanyDocument, data: ConfigUnit): Segment[][] {
  return [
    ["name"],
    ["type"],
    ["purpose"],
    ["lead"],
    ["goals"],
    ["channel"],
    ["knowledge"],
    ...(isConnected(company, "jira") ? [JIRA_PROJECT] : []),
    ...(isConnected(company, "confluence") ? [CONFLUENCE_SPACE] : []),
    ...(schedulesOf(data).length > 0 ? [["schedules"] as Segment[]] : []),
  ];
}

/** The draft unit a unit's key names, and the unit above it. */
function parentUnitOf(api: BuilderApi, key: NodeKey): DraftUnit | undefined {
  const found = locate(api.state.draft, key);
  if (!found || found.parent === COMPANY_KEY) return undefined;
  const parent = locate(api.state.draft, found.parent);
  return parent?.kind === "unit" ? parent.node : undefined;
}

function UnitEditor({ unit, onClose }: { unit: DraftUnit; onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const key = unit.key;
  const { initial, form, set, dirty } = useForm<UnitForm>(() => unitForm(unit.data));
  const { refusal, apply } = useApply(api, key, onClose);
  const { errorFor, rest } = placeOnFields(
    placedOn(api, key),
    unitFieldPaths(state.draft.company, unit.data),
  );
  const disabled = api.readOnly;

  // What the unit inherits when it declares nothing: the lead and channel the
  // unit above it resolved to, as the last check reported them.
  const parent = parentUnitOf(api, key);
  const above = parent ? derivedUnitOf(state, parent.key) : undefined;
  const inheritedLead = above?.lead ? nameOfHandle(state, above.lead) : "";
  const inheritedChannel = above?.channel ?? "";

  const seatNames = [...new Set([...allSeats(state.draft)].map(({ seat }) => seat.data.name))];
  const leadChoices: FieldChoice[] = [
    {
      value: "",
      label:
        inheritedLead && parent
          ? `No lead (inherits ${inheritedLead} from ${parent.data.name})`
          : "No lead",
    },
    ...seatNames.map((name) => ({ value: name, label: name })),
  ];

  const blocked = form.name.trim() === "" ? "A unit needs a name." : null;
  const renamed = form.name.trim() !== initial.name;

  return (
    <EditorShell
      title={`Edit ${unit.data.name || "unit"}`}
      name={unit.data.name || "this unit"}
      dirty={dirty}
      blocked={blocked}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      onApply={() => apply(unitParts(key, initial, form))}
      onClose={onClose}
    >
      <Field
        label="Name"
        value={form.name}
        onChange={(name) => set({ name })}
        disabled={disabled}
        help="Unit names are unique: a lead, a unit reference and a manages entry name exactly one unit."
        error={errorFor(["name"])}
      />
      {renamed && <RenameUnitPreflight unit={key} stored={!isMintedKey(key)} />}
      <UnitTypeField
        value={form.type}
        onChange={(type) => set({ type })}
        error={errorFor(["type"])}
      />
      <Field
        label="Purpose"
        kind="multiline"
        value={form.purpose}
        onChange={(purpose) => set({ purpose })}
        required={false}
        disabled={disabled}
        error={errorFor(["purpose"])}
      />
      <ListField
        label="Goals"
        itemName="goal"
        multiline
        value={form.goals}
        onChange={(goals) => set({ goals })}
        required={false}
        disabled={disabled}
        error={errorFor(["goals"])}
      />

      <EditorSection title="Leadership">
        <Field
          label="Lead"
          kind="choice"
          choices={leadChoices}
          value={form.lead}
          onChange={(lead) => set({ lead })}
          disabled={disabled}
          help="The lead manages the unit's direct members, and a unit below that names no lead inherits this one."
          error={errorFor(["lead"])}
        />
        <Field
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

      <ListField
        label="Knowledge"
        itemName="reference"
        value={form.knowledge}
        onChange={(knowledge) => set({ knowledge })}
        required={false}
        disabled={disabled}
        help="Free-text references, not a read scope."
        error={errorFor(["knowledge"])}
      />

      <Owns
        who="unit"
        jira={form.jira}
        confluence={form.confluence}
        onJira={(jira) => set({ jira })}
        onConfluence={(confluence) => set({ confluence })}
        errorFor={errorFor}
        disabled={disabled}
      />

      <ScheduleToggles
        data={unit.data}
        unit
        values={form.schedules}
        onChange={(name, enabled) => set({ schedules: { ...form.schedules, [name]: enabled } })}
        disabled={disabled}
        error={errorFor(["schedules"])}
      />

      {toolCredentialNames(unit.data).length > 0 && (
        <EditorSection title="Configured in the document">
          <ToolCredentialFact data={unit.data} />
        </EditorSection>
      )}
    </EditorShell>
  );
}

// ---------------------------------------------------------------------------
// A seat
// ---------------------------------------------------------------------------

const GITHUB_TIER: Segment[] = ["integrations", "github", "tier"];
const GITHUB_REPOS: Segment[] = ["integrations", "github", "repos"];
const SLACK_CHANNEL: Segment[] = ["integrations", "slack", "channel"];
const MATTERMOST_CHANNEL: Segment[] = ["integrations", "mattermost", "channel"];
const MATTERMOST_USERNAME: Segment[] = ["integrations", "mattermost", "username"];

/** The field paths a seat's form draws; see [unitFieldPaths] for why it matters. */
function seatFieldPaths(
  company: CompanyDocument,
  data: ConfigRole,
  { human, minted }: { human: boolean; minted: boolean },
): Segment[][] {
  const seatBlock = (tool: "slack" | "mattermost") =>
    isConnected(company, tool) && isRecord(getPath(data, ["integrations", tool]));
  return [
    ["name"],
    // An existing seat's handle is a read-only fact, so a problem about it
    // belongs at the top of the form with the rest.
    ...(minted ? [["handle"] as Segment[]] : []),
    ["email"],
    ["goal"],
    ["backstory"],
    ["responsibilities"],
    ["manages"],
    ...(human
      ? [["contact"] as Segment[], ["availability"] as Segment[]]
      : [
          ["behavioral_guidelines"] as Segment[],
          ["llm"] as Segment[],
          ["token_budget"] as Segment[],
          ...(schedulesOf(data).length > 0 ? [["schedules"] as Segment[]] : []),
          ...(isConnected(company, "github") ? [GITHUB_TIER, GITHUB_REPOS] : []),
          ...(seatBlock("slack") ? [SLACK_CHANNEL] : []),
          ...(seatBlock("mattermost") ? [MATTERMOST_CHANNEL, MATTERMOST_USERNAME] : []),
          ...(isConnected(company, "jira") ? [JIRA_PROJECT] : []),
          ...(isConnected(company, "confluence") ? [CONFLUENCE_SPACE] : []),
        ]),
  ];
}

const GITHUB_TIERS: FieldChoice[] = [
  { value: "", label: "Not set (read only)" },
  { value: "read_only", label: "Read only" },
  { value: "review", label: "Review" },
  { value: "full_access", label: "Full access" },
];

function SeatEditor({ seat, onClose }: { seat: DraftSeat; onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const key = seat.key;
  const data = seat.data;
  const company = state.draft.company;
  const minted = isMintedKey(key);
  const human = kindOf(data) === "human";
  const handle = handleOf(state, key);
  const { initial, form, set, dirty } = useForm<SeatForm>(() =>
    seatForm(data, gitLabAccessLevel(company, handle)),
  );
  const { refusal, apply } = useApply(api, key, onClose);
  const { errorFor, rest } = placeOnFields(
    placedOn(api, key),
    seatFieldPaths(company, data, { human, minted }),
  );
  const disabled = api.readOnly;
  const derived = derivedSeatOf(state, key);

  const budgetError = human ? undefined : tokenBudgetError(form.tokenBudget);
  const blocked =
    form.name.trim() === ""
      ? "A seat needs a name."
      : budgetError
        ? "Correct the token budget first."
        : null;

  return (
    <EditorShell
      title={`Edit ${data.name || "seat"}`}
      name={data.name || "this seat"}
      dirty={dirty}
      blocked={blocked}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      onApply={() => apply(seatParts(key, data, initial, form, { editableHandle: minted }))}
      onClose={onClose}
    >
      <Field
        label="Name"
        value={form.name}
        onChange={(name) => set({ name })}
        disabled={disabled}
        help="Seat names are unique: a lead or a manages entry names exactly one seat."
        error={errorFor(["name"])}
      />
      {minted ? (
        <Field
          label="Handle"
          kind="id"
          value={form.handle}
          onChange={(value) => set({ handle: value })}
          required={false}
          disabled={disabled}
          help={
            form.handle.trim() !== ""
              ? "The handle this seat's memory, mailbox and mentions attach to."
              : derived?.handle
                ? `Empty uses the handle the engine derives from the name: ${derived.handle}, at the last check.`
                : "Empty uses the handle the engine derives from the name, shown here after the next check."
          }
          error={errorFor(["handle"])}
        />
      ) : (
        <ReadOnlyFact
          label="Handle"
          reason="An existing seat keeps its handle: it is the identity its memory and mailbox attach to."
        >
          <code className="inline">{handle ?? ""}</code>
        </ReadOnlyFact>
      )}
      <KindFact seatKey={key} human={human} dirty={dirty} onClose={onClose} />
      <Field
        label="Email"
        kind="email"
        value={form.email}
        onChange={(email) => set({ email })}
        required={false}
        disabled={disabled}
        error={errorFor(["email"])}
      />
      <Field
        label="Goal"
        kind="multiline"
        value={form.goal}
        onChange={(goal) => set({ goal })}
        required={false}
        disabled={disabled}
        error={errorFor(["goal"])}
      />
      <Field
        label="Backstory"
        kind="multiline"
        value={form.backstory}
        onChange={(backstory) => set({ backstory })}
        required={false}
        disabled={disabled}
        error={errorFor(["backstory"])}
      />
      <ListField
        label="Responsibilities"
        itemName="responsibility"
        multiline
        value={form.responsibilities}
        onChange={(responsibilities) => set({ responsibilities })}
        required={false}
        disabled={disabled}
        error={errorFor(["responsibilities"])}
      />
      {!human && (
        <ListField
          label="Behavioral guidelines"
          itemName="guideline"
          multiline
          value={form.guidelines}
          onChange={(guidelines) => set({ guidelines })}
          required={false}
          disabled={disabled}
          error={errorFor(["behavioral_guidelines"])}
        />
      )}

      <Reports
        seat={seat}
        value={form.manages}
        onChange={(manages) => set({ manages })}
        disabled={disabled}
        error={errorFor(["manages"])}
      />

      {human ? (
        <EditorSection
          title="Contact"
          hint="A human seat needs at least one contact identity, which is how the organization reaches the person."
        >
          {CONTACT_IDENTITIES.map(({ key: identity, label }) => (
            <Field
              key={identity}
              label={label}
              kind="id"
              value={form.contact[identity]}
              onChange={(value) => set({ contact: { ...form.contact, [identity]: value } })}
              required={false}
              disabled={disabled}
            />
          ))}
          {errorFor(["contact"]) && <Banner tone="critical">{errorFor(["contact"])}</Banner>}
          <Field
            label="Availability"
            value={form.availability}
            onChange={(availability) => set({ availability })}
            required={false}
            disabled={disabled}
            help="A free-text note, such as working hours."
            error={errorFor(["availability"])}
          />
        </EditorSection>
      ) : (
        <>
          <ModelSection
            data={data}
            chain={form.llm}
            onChain={(llm) => set({ llm })}
            budget={form.tokenBudget}
            onBudget={(tokenBudget) => set({ tokenBudget })}
            budgetError={budgetError ?? errorFor(["token_budget"])}
            chainError={errorFor(["llm"])}
            disabled={disabled}
          />
          <ScheduleToggles
            data={data}
            unit={false}
            values={form.schedules}
            onChange={(name, enabled) => set({ schedules: { ...form.schedules, [name]: enabled } })}
            disabled={disabled}
            error={errorFor(["schedules"])}
          />
          <IntegrationsSection
            data={data}
            handle={handle}
            minted={minted}
            initial={initial}
            form={form}
            set={set}
            errorFor={errorFor}
            disabled={disabled}
          />
          <Owns
            who="seat"
            jira={form.jira}
            confluence={form.confluence}
            onJira={(jira) => set({ jira })}
            onConfluence={(confluence) => set({ confluence })}
            errorFor={errorFor}
            disabled={disabled}
          />
          <DocumentFacts data={data} handle={handle} />
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
          size="sm"
          disabled={dirty || api.readOnly}
          onClick={() => {
            onClose();
            api.openChangeKind(seatKey);
          }}
        >
          {human ? "Change to an agent seat" : "Change to a human seat"}
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
}: {
  seat: DraftSeat;
  value: readonly string[];
  onChange: (next: string[]) => void;
  disabled: boolean;
  error?: string;
}) {
  const { state } = useBuilder();
  const seatNames = new Set<string>();
  const options: PickerOption[] = [];
  for (const { seat: other } of allSeats(state.draft)) {
    if (other.key === seat.key || seatNames.has(other.data.name)) continue;
    seatNames.add(other.data.name);
    options.push({ value: other.data.name, label: other.data.name, group: "Seats" });
  }
  for (const { unit } of allUnits(state.draft)) {
    // A manages entry that names both a seat and a unit names the seat, so a
    // unit sharing a seat's name is not offered as a second meaning of it.
    if (seatNames.has(unit.data.name) || unit.data.name === seat.data.name) continue;
    options.push({
      value: unit.data.name,
      label: unit.data.name,
      group: "Units",
      hint: "every seat in it",
    });
  }

  const derived = derivedSeatOf(state, seat.key);
  const automatic = new Set(derived?.auto_reports ?? []);
  const groups = (state.check.derived?.units ?? [])
    .filter((u) => derived?.handle && u.lead === derived.handle)
    .map((u) => ({
      unit: u.name,
      names: (u.seats ?? []).filter((h) => automatic.has(h)).map((h) => nameOfHandle(state, h)),
    }))
    .filter((g) => g.names.length > 0);

  return (
    <EditorSection title="Reports">
      <MultiPicker
        label="Manages"
        options={options}
        value={value}
        onChange={onChange}
        required={false}
        disabled={disabled}
        help="The seats this seat manages, or a unit to manage every seat in it."
        error={error}
      />
      {groups.map((g) => (
        <ReadOnlyFact
          key={g.unit}
          label={`Managed automatically as lead of ${g.unit}`}
          reason="A unit lead manages the unit's direct members unless another member manages them. Change the unit's lead to change this."
        >
          {g.names.join(", ")}
        </ReadOnlyFact>
      ))}
    </EditorSection>
  );
}

function ModelSection({
  data,
  chain,
  onChain,
  budget,
  onBudget,
  budgetError,
  chainError,
  disabled,
}: {
  data: ConfigRole;
  chain: readonly string[] | null;
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
      {chain === null ? (
        <ReadOnlyFact
          label="Model"
          reason="This seat chooses a model per phase."
          link={{ to: ["config"], label: "Edit in the configuration document" }}
        >
          <ul className="builder-list">
            {formatPhaseLLM(data.llm).map((row) => (
              <li key={row.phase}>
                {row.phase ? `${row.phase}: ` : ""}
                {row.chain}
              </li>
            ))}
          </ul>
        </ReadOnlyFact>
      ) : (
        <MultiPicker
          label="Model"
          options={providers.map((key) => ({ value: key, label: key }))}
          value={chain}
          onChange={onChain}
          required={false}
          disabled={disabled}
          help={
            chain.length > 0
              ? `Tried in this order: ${chain.join(", then ")}. To change the order, remove a provider and choose it again.`
              : unpinned
                ? `Runs on ${unpinned}, the provider a seat that names none runs on.`
                : "The company has no model provider yet. Add one in the configuration document."
          }
          error={chainError}
        />
      )}
      <Field
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
  data,
  handle,
  minted,
  initial,
  form,
  set,
  errorFor,
  disabled,
}: {
  data: ConfigRole;
  handle: string | undefined;
  minted: boolean;
  initial: SeatForm;
  form: SeatForm;
  set: (patch: Partial<SeatForm>) => void;
  errorFor: (path: readonly Segment[]) => string | undefined;
  disabled: boolean;
}) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const github = getPath(data, ["integrations", "github"]);
  const appSlug = isRecord(github) && typeof github.app_slug === "string" ? github.app_slug : "";
  const enrolling = !isRecord(github) && (form.githubTier !== "" || form.githubRepos.length > 0);
  const tierChanged = appSlug !== "" && form.githubTier !== initial.githubTier;
  const slack = getPath(data, ["integrations", "slack"]);
  const mattermost = getPath(data, ["integrations", "mattermost"]);
  const provisioned =
    isRecord(mattermost) && typeof mattermost.bot_token === "string" && mattermost.bot_token !== "";
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
                <code className="inline">{appSlug}</code>
              </ReadOnlyFact>
            )}
            <Field
              label="Access tier"
              kind="choice"
              choices={GITHUB_TIERS}
              value={form.githubTier}
              onChange={(githubTier) => set({ githubTier })}
              disabled={disabled}
              error={errorFor(GITHUB_TIER)}
            />
            <ListField
              label="Repositories"
              itemName="repository"
              value={form.githubRepos}
              onChange={(githubRepos) => set({ githubRepos })}
              required={false}
              disabled={disabled}
              placeholder="owner/name"
              help="Empty means every repository the installation covers."
              error={errorFor(GITHUB_REPOS)}
            />
            {enrolling && (
              <Banner tone="info">
                This enrols the seat in GitHub. Create its app from Integrations.{" "}
                <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>
              </Banner>
            )}
            {tierChanged && (
              <Banner tone="caution">
                The app's permissions were fixed when it was created. Raise them at GitHub as well.
              </Banner>
            )}
          </>
        ) : (
          <NotConnected tool="github" />
        )}
      </EditorSection>

      <EditorSection title="Slack">
        {!isConnected(company, "slack") ? (
          <NotConnected tool="slack" />
        ) : isRecord(slack) ? (
          <Field
            label="Slack channel ID"
            kind="id"
            value={form.slackChannel}
            onChange={(slackChannel) => set({ slackChannel })}
            required={false}
            disabled={disabled}
            help="The ID of this seat's default channel, such as C0123ABCD, not its name."
            error={errorFor(SLACK_CHANNEL)}
          />
        ) : (
          <p className="builder-note muted">
            This seat has no Slack app of its own, so it has no channel to set.{" "}
            <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>
          </p>
        )}
      </EditorSection>

      <EditorSection title="Mattermost">
        {!isConnected(company, "mattermost") ? (
          <NotConnected tool="mattermost" />
        ) : isRecord(mattermost) ? (
          <>
            <Field
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
                reason="The bot is provisioned under this name. Changing it would make the provisioner find or create a second bot."
              >
                <code className="inline">{form.mattermostUsername || handle || ""}</code>
              </ReadOnlyFact>
            ) : (
              <Field
                label="Bot username"
                kind="id"
                value={form.mattermostUsername}
                onChange={(mattermostUsername) => set({ mattermostUsername })}
                required={false}
                disabled={disabled}
                help="Set it only when the bot account already exists under another name. Empty uses the seat's handle."
                error={errorFor(MATTERMOST_USERNAME)}
              />
            )}
          </>
        ) : (
          <p className="builder-note muted">
            This seat has no Mattermost bot of its own, so it has no channel to set.{" "}
            <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>
          </p>
        )}
      </EditorSection>

      <EditorSection title="GitLab">
        {!isConnected(company, "gitlab") ? (
          <NotConnected tool="gitlab" />
        ) : !hasGitLabProvisioning(company) ? (
          <p className="builder-note muted">
            GitLab provisioning is not set up, so there is no access level to set.{" "}
            <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>
          </p>
        ) : (
          <Field
            label="Access level"
            kind="choice"
            choices={[
              { value: "", label: `Company default (${gitlabDefault})` },
              { value: "developer", label: "Developer" },
              { value: "maintainer", label: "Maintainer" },
            ]}
            value={form.accessLevel}
            onChange={(accessLevel) => set({ accessLevel })}
            disabled={disabled || handle === undefined}
            help={
              handle === undefined
                ? "Access levels are kept by handle, so this is available once the check reports this seat's handle."
                : minted
                  ? "Kept by handle: choosing a handle for this seat carries its level with it."
                  : "The level this seat's GitLab account joins the group with."
            }
          />
        )}
      </EditorSection>
    </EditorSection>
  );
}

/** The seat's settings the builder shows and does not edit, each with why. */
function DocumentFacts({ data, handle }: { data: ConfigRole; handle: string | undefined }) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const facts: ReactNode[] = [];
  const configLink = { to: ["config"], label: "Open the configuration" };

  const phases = PHASE_MODEL_FIELDS.filter((field) => data[field] !== undefined);
  if (phases.length > 0) {
    facts.push(
      <ReadOnlyFact
        key="phases"
        label="Models per phase"
        reason="Set in the configuration document."
        link={configLink}
      >
        <ul className="builder-list">
          {phases.map((field) => (
            <li key={field}>
              {field.replace("llm_", "")}:{" "}
              {formatPhaseLLM(data[field])
                .map((r) => r.chain)
                .join("; ")}
            </li>
          ))}
        </ul>
      </ReadOnlyFact>,
    );
  }
  if (isRecord(data.sandbox)) {
    const enabled = data.sandbox.enabled === true ? "Enabled" : "Not enabled";
    const runIn = typeof data.sandbox.run_in === "string" ? `, runs in ${data.sandbox.run_in}` : "";
    facts.push(
      <ReadOnlyFact
        key="sandbox"
        label="Sandbox"
        reason="A sandbox runs on the company's providers.sandbox block, which the builder does not edit."
        link={configLink}
      >
        {enabled}
        {runIn}
      </ReadOnlyFact>,
    );
  }
  if (Array.isArray(data.workers) && data.workers.length > 0) {
    facts.push(
      <ReadOnlyFact
        key="workers"
        label="Workers"
        reason="Workers name templates from the company's workers block, which the builder does not edit."
        link={configLink}
      >
        {data.workers.join(", ")}
      </ReadOnlyFact>,
    );
  }
  if (isRecord(data.placement)) {
    facts.push(
      <ReadOnlyFact
        key="placement"
        label="Placement"
        reason="Placement is a fleet setting for which nodes run this seat, edited in the configuration document."
        link={configLink}
      >
        {Object.entries(data.placement)
          .map(([k, v]) => `${k}: ${Array.isArray(v) ? v.join(", ") : String(v)}`)
          .join("; ")}
      </ReadOnlyFact>,
    );
  }
  if (typeof data.learning_enabled === "boolean") {
    facts.push(
      <ReadOnlyFact
        key="learning"
        label="Learning"
        reason="Set in the configuration document."
        link={configLink}
      >
        {data.learning_enabled ? "On" : "Off"}
      </ReadOnlyFact>,
    );
  }
  if (toolCredentialNames(data).length > 0)
    facts.push(<ToolCredentialFact key="mcp" data={data} />);
  if (isConnected(company, "datadog")) {
    const fallback = handle !== undefined && datadogFallback(company) === handle;
    facts.push(
      <ReadOnlyFact
        key="datadog"
        label="Datadog fallback"
        reason="An alert whose tags name no seat wakes the fallback seat. It is chosen from Integrations, or when the fallback seat is deleted."
        link={{ to: ["integrations"], label: "Open Integrations" }}
      >
        {fallback ? "This seat is the Datadog fallback." : "Not the Datadog fallback."}
      </ReadOnlyFact>,
    );
  }
  if (facts.length === 0) return null;
  return (
    <EditorSection
      title="Configured in the document"
      hint={`${plural(facts.length, "setting")} the builder shows and does not edit.`}
    >
      {facts}
    </EditorSection>
  );
}
