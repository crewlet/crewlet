/**
 * The node editor: a unit, a seat, or the company's name, in a side sheet.
 *
 * FORM-LOCAL, APPLIED AS ONE OPERATION. Every field is held in a local form
 * until Apply, which records the fields that changed as one `edit` (see
 * `editorForm.ts`): nothing reaches the draft while a sentence is half
 * typed, a refusal keeps the form open with the engine's reason on screen,
 * and Undo takes back the whole of what Apply did. Closing a form with
 * changes in it, by Cancel, Close, Escape or the veil, asks first, because a
 * form's changes exist nowhere else, and so does every move that leaves the
 * builder (one of its links, Back or Forward, a reload): the form holds
 * a leave guard (`app/router.useLeaveGuard`) for as long as it has changes.
 * A move within the builder (Back from the outline to the canvas) keeps the
 * editor open with its form, so it is let go without a question
 * (`BuilderContext.keepsTheLens`).
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
 * Mattermost channel and GitLab access level mean something only when the
 * company has connected that tool, so each says "<Tool> is not
 * connected" instead of offering a setting that does nothing. A project and a
 * space are NOT a tool's: they are the seat's or unit's own `project` and
 * `space`, naming a container on whichever tracker and knowledge base the
 * company runs — the engine's own by default — so they are drawn unless the
 * company runs none. Two of the tool fields change more than their field,
 * and say so beside it: a GitHub tier on a seat with no GitHub block enrols
 * the seat, and a tier change on a seat whose app exists does not reach the
 * app, whose permissions were fixed when it was created.
 *
 * NO CREDENTIAL IS RENDERED. Tool credentials appear as server and variable
 * names, a seat's chat app blocks as the settings a person writes (a channel,
 * a username), and nothing else of theirs.
 *
 * THE ENGINE'S PROBLEMS SIT BESIDE THE FIELDS THEY NAME, placed through the
 * same path index the check's answer was placed with; whatever names no
 * drawn field is listed at the top, so nothing the engine said is dropped.
 * A warning, which the engine accepts, is listed at the top as a caution
 * rather than drawn as a field's error (see `placeOnFields`).
 */

import { useId, useRef, useState, type ReactNode } from "react";
import { useLeaveGuard } from "~/app/router.tsx";
import type { CompanyDocument, ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { formatPhaseLLM, plural } from "~/lib/format.ts";
import { ConfigField, HelperOnFocus, type FieldChoice } from "~/components/ConfigField.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { isWholeReference } from "~/lib/references.ts";
import { handleLabel } from "~/lib/seats.ts";
import { sandboxWords } from "~/routes/agents/seat/profile.ts";
import { useViewer } from "~/lib/viewer.ts";
import { SeatHolding, canManagePeople, useHumanSeats } from "~/components/people.tsx";
import { TOOL_TONE, toolRollup } from "~/lib/toolState.ts";
import type { IntegrationTool } from "~/contract/integrations.ts";
import { AVATAR_CHARACTERS } from "~/contract/avatar.ts";
import type { HumanContactKey } from "~/protocol/index.ts";
import { VendorMark, type Vendor } from "@crewlethq/icons";
import { CREWLET_CHARACTER_GEOMETRY } from "@crewlethq/icons/characters";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import {
  keepsTheLens,
  useBuilder,
  type BuilderApi,
  type EditorPreset,
  type EditorSectionName,
} from "./BuilderContext.tsx";
import {
  companyForm,
  companyParts,
  editIntent,
  renames,
  scheduleRuns,
  schedulesOf,
  seatForm,
  seatParts,
  tokenBudgetErrors,
  unitForm,
  unitParts,
  type BudgetForm,
  type BudgetWindow,
  type CompanyForm,
  type SeatForm,
  type UnitForm,
} from "./editorForm.ts";
import {
  ACKNOWLEDGEMENT_TEXT,
  EditorSection,
  NodeProblems,
  ReadOnlyFact,
  Refusal,
  ScreenLink,
  IDENTITY_FIXED,
  IDENTITY_HELP,
  NAME_HELP,
  UnitTypeField,
  placeOnFields,
  unitTypeLabel,
} from "./dialogParts.tsx";
import { NodeGlyph } from "~/ui/orgNodes.tsx";
import type { AvatarCharacter } from "~/lib/avatar.ts";
import type { Segment } from "./model/document.ts";
import {
  allUnits,
  handleOf,
  locate,
  unitIdOf,
  type DraftSeat,
  type DraftUnit,
} from "./model/draft.ts";
import { mintHandle } from "./model/identity.ts";
import { getPath, isRecord, jsonEqual } from "./model/json.ts";
import { COMPANY_KEY, isMintedKey, mintKey, type NodeKey } from "./model/keys.ts";
import { kindOf, withoutMasked, type EditPartIntent } from "./model/operations.ts";
import type { PlacedProblem } from "./model/problems.ts";
import { recordIntent } from "./model/reducer.ts";
import { scopeLimit } from "./model/scope.ts";
import { CONTACT_IDENTITIES } from "./model/templates.ts";
import { datadogEnabled, datadogFallback } from "./chartModel.ts";
import {
  PHASE_MODEL_FIELDS,
  TOOL_NAMES,
  backendFor,
  currentCheck,
  defaultGitLabAccessLevel,
  derivedSeatOf,
  derivedUnitOf,
  gitLabAccessLevel,
  hasGitLabProvisioning,
  identityProblemOf,
  isConnected,
  mattermostBotUsername,
  nameOfHandle,
  placementSummary,
  providerOrder,
  seatLabels,
  takenIdentities,
  toolCredentialNames,
  unpinnedProvider,
} from "./nodeFacts.ts";
import {
  AvatarPicker,
  Button,
  Callout,
  Checkbox,
  ConfirmModal,
  EmptyState,
  FactList,
  FormField,
  InlineCode,
  ListInput,
  Modal,
  NODE_HUE_NAMES,
  Tabs,
  Tag,
  TagsInput,
  VisuallyHidden,
  tabId,
  type FactListItem,
  type TabsMarker,
} from "@crewlethq/ui";
import type { TagsInputOption } from "@crewlethq/ui";
import { withProblems } from "~/ui/Problems.tsx";

export function NodeEditor({
  nodeKey,
  section,
  preset,
  onClose,
}: {
  nodeKey: NodeKey;
  /** The part of the form to start on (see `EditorSectionName`). */
  section?: EditorSectionName;
  /** Values the form starts holding on top of the node's own (see `EditorPreset`). */
  preset?: EditorPreset;
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
          description="It was undone or replaced. Choose another node."
        />
      </Modal>
    );
  }
  // ONE FORM PER OPENING, NOT PER KEY. The Builder mounts a new editor for
  // every opening (its dialog host keys each dialog by the opening), so
  // another node, or the same one opened again, starts from its own data. A
  // node's key can also change while its editor is open (the first check keys
  // the base by the engine's handles), and the form and its drawer stay: keyed
  // by the node here, they were torn down and built again on that answer.
  return found.kind === "unit" ? (
    <UnitEditor unit={found.node} section={section} preset={preset} onClose={onClose} />
  ) : (
    <SeatEditor seat={found.node} section={section} preset={preset} onClose={onClose} />
  );
}

// ---------------------------------------------------------------------------
// The shell every form shares
// ---------------------------------------------------------------------------

/**
 * A form that starts from `initial` and knows whether it has been changed.
 *
 * `arrived` is what an address asked it to hold on top (`EditorPreset`):
 * applied to the FORM and not to `initial`, so it reads as a change the
 * reader has still to apply, and the editor's Apply records it like any edit.
 */
function useForm<T>(initial: () => T, arrived?: (start: T) => Partial<T> | undefined) {
  const [start] = useState(initial);
  const [form, setForm] = useState(() => ({ ...start, ...arrived?.(start) }));
  return {
    initial: start,
    form,
    set: (patch: Partial<T>) => setForm((was) => ({ ...was, ...patch })),
    dirty: !jsonEqual(start, form),
  };
}

/**
 * The project an address asked a node's editor to hold, for a node that owns
 * none yet: one that already owns a project keeps it.
 */
function presetProject<T extends { project: string }>(
  preset: EditorPreset | undefined,
): (start: T) => Partial<T> | undefined {
  return (start) =>
    preset?.project && start.project === ""
      ? ({ project: preset.project } as Partial<T>)
      : undefined;
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
 * form is read-only right now; repeating that sentence on the button reads it
 * out twice to anybody using a screen reader, once on entering the form and
 * again on reaching the control.
 */
const READ_ONLY_REASON = "Read-only.";

/**
 * One tab of a node's editor: its label, the field paths it draws (which is how
 * a problem, a warning or a change finds its tab) and its content.
 */
export interface EditorTab {
  id: EditorTabId;
  label: string;
  paths: readonly (readonly Segment[])[];
  content: ReactNode;
}

/** Every tab any editor draws, so a deep link and a test can name one. */
export type EditorTabId =
  "profile" | "work" | "model" | "integrations" | "advanced" | "contact" | "leadership";

/** Whether `path` lies at or below `owner`. */
function within(path: readonly Segment[], owner: readonly Segment[]): boolean {
  return owner.length <= path.length && owner.every((segment, i) => path[i] === segment);
}

/** The tab a path belongs to: the one owning the longest prefix of it. */
function tabOf(tabs: readonly EditorTab[], path: readonly Segment[]): EditorTabId | undefined {
  let best: { id: EditorTabId; length: number } | undefined;
  for (const tab of tabs) {
    for (const owner of tab.paths) {
      if (within(path, owner) && (best === undefined || owner.length > best.length)) {
        best = { id: tab.id, length: owner.length };
      }
    }
  }
  return best?.id;
}

/**
 * Each tab's marker: its problems as a count, else a warning, else a change.
 * ONE PER TAB, the most urgent, which is the kit's own rule for a marker.
 */
function markersOf(
  tabs: readonly EditorTab[],
  placed: readonly Pick<PlacedProblem, "field" | "severity">[],
  changed: readonly (readonly Segment[])[],
): Map<EditorTabId, TabsMarker> {
  const problems = new Map<EditorTabId, number>();
  const warned = new Set<EditorTabId>();
  for (const p of placed) {
    const tab = tabOf(tabs, p.field);
    if (tab === undefined) continue;
    if (p.severity === "problem") problems.set(tab, (problems.get(tab) ?? 0) + 1);
    else warned.add(tab);
  }
  const touched = new Set(changed.map((path) => tabOf(tabs, path)).filter((t) => t !== undefined));
  const out = new Map<EditorTabId, TabsMarker>();
  for (const tab of tabs) {
    const count = problems.get(tab.id) ?? 0;
    if (count > 0) out.set(tab.id, { tone: "danger", count, label: plural(count, "problem") });
    else if (warned.has(tab.id)) out.set(tab.id, { tone: "warning", label: "A warning" });
    else if (touched.has(tab.id)) out.set(tab.id, { tone: "changed", label: "Changed" });
  }
  return out;
}

/** The paths an edit's parts change, for the tabs' "changed" markers. */
function changedPaths(parts: readonly EditPartIntent[]): Segment[][] {
  const out: Segment[][] = [];
  for (const part of parts) {
    switch (part.type) {
      case "renameSeat":
      case "renameUnit":
        out.push(["name"]);
        break;
      case "setLead":
        out.push(["lead"]);
        break;
      case "setManages":
        out.push(["manages"]);
        break;
      case "setScheduleEnabled":
        out.push(["schedules"]);
        break;
      default:
        if ("set" in part) for (const field of part.set) out.push([...field.path]);
        if ("accessLevel" in part && part.accessLevel !== undefined) out.push(ACCESS_LEVEL);
    }
  }
  return out;
}

/** What an editor's panel focuses first: its first field, whatever kind of control it is. */
const FIRST_FIELD =
  "input:not([type=hidden]), textarea, select, [role=combobox], [role=radio][aria-checked=true]";

/** Where a seat's GitLab access level is said to live, for its tab's marker. */
const ACCESS_LEVEL: Segment[] = ["integrations", "gitlab", "access_level"];

function EditorShell({
  title,
  subtitle,
  icon,
  name,
  dirty,
  blocked,
  blockedPath,
  readOnly,
  refusal,
  problems,
  placed,
  changed,
  tabs,
  initialTab,
  onApply,
  onClose,
  children,
}: {
  /** The node's name as the form holds it now, so the header follows the Name box. */
  title: string;
  /** The handle and the kind, under the name. Never where the node sits: that is Work's. */
  subtitle?: ReactNode;
  /** The node's own mark, or an agent's avatar picker. */
  icon: ReactNode;
  /** How the node is named in the discard prompt. */
  name: string;
  dirty: boolean;
  /** Why Apply is unavailable for what the form holds, or `null`. */
  blocked: string | null;
  /** The field the block is about, so pressing Apply opens its tab. */
  blockedPath?: readonly Segment[] | undefined;
  readOnly: boolean;
  refusal: string | null;
  /** What no field of this form owns, listed at the top of every tab. */
  problems: readonly PlacedProblem[];
  /** Every problem and warning on the node, for the tabs' markers. */
  placed: readonly PlacedProblem[];
  /** The paths the form has changed, for the tabs' markers. */
  changed: readonly (readonly Segment[])[];
  /** The form's tabs; a form short enough for one panel passes `children` instead. */
  tabs?: readonly EditorTab[];
  initialTab?: EditorTabId | undefined;
  onApply: () => void;
  onClose: () => void;
  children?: ReactNode;
}) {
  // `leave` is the move the router held (a link, Back or Forward) when the
  // question came from one, so the same question serves a close and a move,
  // and discarding then makes the move. Only a move off the builder is held: the
  // Builder keeps this editor, form and all, through a move within it.
  const [confirming, setConfirming] = useState<{ leave: (() => void) | null } | null>(null);
  const [tab, setTab] = useState<EditorTabId | undefined>(
    () => tabs?.find((t) => t.id === initialTab)?.id ?? tabs?.[0]?.id,
  );
  const panelId = useId();
  const panel = useRef<HTMLDivElement | null>(null);
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
  // WHAT BLOCKS APPLY IS A PROBLEM TOO, though no check reported it: a ceiling
  // that is not a number is refused by the form itself, and the tab holding it
  // is where the reader has to go.
  const marked =
    blocked !== null && !readOnly && blockedPath
      ? [...placed, { field: blockedPath, severity: "problem" as const }]
      : placed;
  const markers = tabs ? markersOf(tabs, marked, changed) : new Map<EditorTabId, TabsMarker>();
  const active = tabs?.find((t) => t.id === tab) ?? tabs?.[0];
  return (
    <>
      <Modal
        open
        variant="sheet"
        flush
        // NAMED FOR WHAT IT DOES: the header shows the node's name, and a
        // reader's software hears "Edit" before it, as every editor here is named.
        title={
          <>
            <VisuallyHidden>Edit</VisuallyHidden> {title}
          </>
        }
        subtitle={subtitle}
        icon={icon}
        onClose={requestClose}
        // THE FORM'S FIRST FIELD, never the tab row above it: a reader opens an
        // editor to change something, and a field a deep link asked for (the
        // Manages picker, a unit's lead) has already taken focus on mount.
        initialFocus={() => {
          const active = document.activeElement;
          if (active instanceof HTMLElement && panel.current?.contains(active)) return active;
          return panel.current?.querySelector<HTMLElement>(FIRST_FIELD) ?? null;
        }}
        // ALWAYS A FORM. The panel is a <form> only while it has a submit
        // handler, and swapping the element as Apply became unavailable would
        // remount every field in it, dropping focus and a list's edit in
        // progress. The guard lives in the handler instead, and a press that
        // the form refuses opens the tab holding the reason.
        onSubmit={() => {
          if (!disabled) return onApply();
          const owner = tabs && blockedPath ? tabOf(tabs, blockedPath) : undefined;
          if (owner) setTab(owner);
        }}
        // COMMITTED FROM THE HEAD, as the console's edit panel is: the header
        // and the tabs stay put while the panel scrolls, so Apply is one press
        // away from any field. With Cancel in the band there is no close
        // control beside it doing the same thing.
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
               * was.
               */
              disabledReason={disabled ? reason : undefined}
            >
              Apply
            </Button>
          </>
        }
        footerStart={disabled ? reason : undefined}
      >
        <HelperOnFocus.Provider value>
          <div className="node-editor">
            {tabs && active && (
              <div className="node-editor-tabs">
                <Tabs
                  variant="underline"
                  ariaLabel="Sections"
                  panelId={panelId}
                  value={active.id}
                  onValueChange={(next) => setTab(next as EditorTabId)}
                  items={tabs.map((t) => ({
                    value: t.id,
                    label: t.label,
                    marker: markers.get(t.id),
                  }))}
                />
              </div>
            )}
            {/* THE PANEL IS THE ACTIVE TAB'S, labelled by it: one tabpanel the
                row controls. A form with one panel draws its fields here too. */}
            <div
              ref={panel}
              className="node-editor-panel"
              {...(tabs && active
                ? { role: "tabpanel", id: panelId, "aria-labelledby": tabId(panelId, active.id) }
                : {})}
            >
              {readOnly && <Callout variant="neutral">Read-only right now.</Callout>}
              <Refusal message={refusal} />
              <NodeProblems problems={problems} />
              {active ? active.content : children}
            </div>
          </div>
        </HelperOnFocus.Provider>
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
        message={`Your edits to ${name} aren’t in the draft yet.`}
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
    <EditorSection title="Schedules" aside={<ScreenLink to="schedules">Open Schedules</ScreenLink>}>
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

/** Tool credentials as one fact row: server names and variable names, never values. */
function toolCredentialItem(names: ReturnType<typeof toolCredentialNames>): FactListItem {
  return {
    key: "mcp",
    label: "Tool credentials",
    value: names
      .map(({ server, variables }) =>
        variables.length > 0 ? `${server} (${variables.join(", ")})` : server,
      )
      .join(", "),
    reason: "Set in the configuration file.",
  };
}

/** A unit's tool credentials, as one fact row. */
function ToolCredentialFact({ data }: { data: ConfigRole | ConfigUnit }) {
  const names = toolCredentialNames(data);
  if (names.length === 0) return null;
  return <FactList items={[toolCredentialItem(names)]} />;
}

const PROJECT: Segment[] = ["project"];
const SPACE: Segment[] = ["space"];

/** The ownership fields the company has a backend for, as field paths. */
function ownsFieldPaths(company: CompanyDocument): Segment[][] {
  return [
    ...(backendFor(company, "tracker") !== "none" ? [PROJECT] : []),
    ...(backendFor(company, "knowledge") !== "none" ? [SPACE] : []),
  ];
}

/**
 * The tracker project and knowledge space a unit or a root seat OWNS: where
 * its unrouted work is filed and where it writes pages, on whichever backend
 * runs the tracker and the knowledge base. Vendor-neutral fields of the
 * document (`project`, `space`), never a vendor's block, which no unit and no
 * seat carries. Not a permission. Each is drawn only where the company runs
 * that backend: with none, there is nothing to own, and the section says so.
 */
function Owns({
  who,
  project,
  space,
  onProject,
  onSpace,
  errorFor,
  disabled,
  keyLimit,
  focusProject = false,
}: {
  who: "seat" | "unit";
  project: string;
  space: string;
  onProject: (next: string) => void;
  onSpace: (next: string) => void;
  errorFor: (path: readonly Segment[]) => string | undefined;
  disabled: boolean;
  /** Why a lead's draft may not change either (`model/scope.ts`), or `null`. */
  keyLimit: string | null;
  /** Start on the project field: the editor was opened at its Project part. */
  focusProject?: boolean;
}) {
  const { state } = useBuilder();
  const company = state.draft.company;
  return (
    <EditorSection title="Owns" hint={keyLimit ?? undefined}>
      <div className="builder-pair">
        {backendFor(company, "tracker") !== "none" ? (
          <ConfigField
            label="Tracker project"
            kind="id"
            value={project}
            onChange={onProject}
            required={false}
            disabled={disabled || keyLimit !== null}
            autoFocus={focusProject}
            help={
              who === "unit"
                ? "Where unrouted work for this unit goes. Not a permission."
                : "Where unrouted work for this seat goes, for a seat at the top level: a seat inside a unit takes its unit's. Not a permission."
            }
            error={errorFor(PROJECT)}
          />
        ) : (
          <p className="builder-note muted">No work tracker is set up.</p>
        )}
        {backendFor(company, "knowledge") !== "none" ? (
          <ConfigField
            label="Knowledge space"
            kind="id"
            value={space}
            onChange={onSpace}
            required={false}
            disabled={disabled || keyLimit !== null}
            help={`Where this ${who} writes pages nobody else is named for. Not a permission.`}
            error={errorFor(SPACE)}
          />
        ) : (
          <p className="builder-note muted">No knowledge base is set up.</p>
        )}
      </div>
    </EditorSection>
  );
}

// ---------------------------------------------------------------------------
// The company
// ---------------------------------------------------------------------------

const COMPANY_FIELDS: Segment[][] = [["name"]];

/**
 * Renaming the company: the one company field the builder writes.
 *
 * THE COMPANY'S PRINCIPLES ARE NOT HERE. Its mission, vision and standing
 * policies are written on Company › Principles, in place, and the company
 * node's Edit goes there (`Builder.tsx`'s `openEditor`). What stays is the
 * name, because a rename is an organization change rather than a principle:
 * the engine derives every agent's id from it and every seat onboards again,
 * which is what this form's acknowledgement and the review's rename cause
 * say. So the form is reached by the company node's Rename company.
 */
function CompanyEditor({ onClose }: { onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const { initial, form, set, dirty } = useForm<CompanyForm>(() =>
    companyForm(state.draft.company),
  );
  const { refusal, apply } = useApply(api, COMPANY_KEY, onClose);
  const [acknowledged, setAcknowledged] = useState(false);
  const { errorFor, rest } = placeOnFields(placedOn(api, COMPANY_KEY), COMPANY_FIELDS);
  // A LEAD'S DRAFT of one unit shows the company's name and may not change it:
  // that is the company grant's.
  const limit = scopeLimit(state.scope, state.draft, COMPANY_KEY, "edit");
  const disabled = api.readOnly || limit !== null;

  // ASKED OF THE FORM THAT RENAMES, AND ONLY THEN. A name nobody typed in (a
  // draft renamed by an earlier edit, a stored name with a space around it)
  // asks nothing. It compares with the SAVED name as the engine holds it,
  // untrimmed, because the engine derives agent ids from exactly that string,
  // and a name typed back to the saved one is no rename at all.
  const savedName = typeof state.base.document?.name === "string" ? state.base.document.name : "";
  const renaming =
    savedName !== "" && renames(initial.name, form.name) && form.name.trim() !== savedName;
  const blocked =
    limit ??
    (form.name.trim() === ""
      ? "The company needs a name."
      : renaming && !acknowledged
        ? "Confirm what renaming the company does."
        : null);

  const parts = companyParts(initial, form);
  return (
    <EditorShell
      title={form.name.trim() || "Company"}
      subtitle="Company name"
      icon={<NodeGlyph kind="company" size="md" />}
      name="the company's name"
      dirty={dirty}
      blocked={blocked}
      blockedPath={["name"]}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      placed={placedOn(api, COMPANY_KEY)}
      changed={changedPaths(parts)}
      onApply={() => apply(parts)}
      onClose={onClose}
    >
      <ConfigField
        label="Company name"
        value={form.name}
        onChange={(name) => set({ name })}
        autoFocus={!disabled}
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
      <p className="builder-note muted">
        The mission, the vision and the standing policies are written on{" "}
        <ScreenLink to="principles">Company › Principles</ScreenLink>.
      </p>
    </EditorShell>
  );
}

// ---------------------------------------------------------------------------
// A unit
// ---------------------------------------------------------------------------

/**
 * The field paths a unit's form draws, so a problem the engine reported on a
 * field this form does NOT draw (a project on a company that runs no tracker)
 * is listed at the top rather than attached to a field nobody can see.
 */
function unitFieldPaths(
  company: CompanyDocument,
  data: ConfigUnit,
  { minted }: { minted: boolean },
): Segment[][] {
  return [
    ["name"],
    // A saved unit's key is a read-only fact, so a problem about it belongs at
    // the top of the form with the rest; a key this draft chose is a field.
    ...(minted ? [["id"] as Segment[]] : []),
    ["type"],
    ["purpose"],
    ["lead"],
    ["goals"],
    ["channel"],
    ["knowledge"],
    ...ownsFieldPaths(company),
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

function UnitEditor({
  unit,
  section,
  preset,
  onClose,
}: {
  unit: DraftUnit;
  section: EditorSectionName | undefined;
  preset: EditorPreset | undefined;
  onClose: () => void;
}) {
  const api = useBuilder();
  const { state } = api;
  const key = unit.key;
  const { initial, form, set, dirty } = useForm<UnitForm>(
    () => unitForm(unit.data),
    presetProject(preset),
  );
  const { refusal, apply } = useApply(api, key, onClose);
  const minted = isMintedKey(key);
  const { errorFor, rest } = placeOnFields(
    placedOn(api, key),
    unitFieldPaths(state.draft.company, unit.data, { minted }),
  );
  const disabled = api.readOnly;
  // A LEAD'S OWN UNIT keeps its lead, which decides who may change the unit,
  // and every unit keeps the keys another system finds it by
  // (`model/scope.ts`); every other field stays theirs to edit.
  const leadLimit = scopeLimit(state.scope, state.draft, key, "lead");
  const keyLimit = scopeLimit(state.scope, state.draft, key, "key");

  // What the unit inherits when it declares nothing: the lead and channel the
  // unit above it resolved to, as the last check reported them.
  const parent = parentUnitOf(api, key);
  const above = parent ? derivedUnitOf(state, parent.key) : undefined;
  const inheritedLead = above?.lead ? nameOfHandle(state, above.lead) : "";
  const inheritedChannel = above?.channel ?? "";

  // A LEAD IS WRITTEN AS A HANDLE and offered by name. One the draft holds no
  // seat of stays on offer as what it is, or the choice would show nothing
  // and the first change of the form would silently clear it.
  const labels = seatLabels(state.draft);
  const leadChoices: FieldChoice[] = [
    {
      value: "",
      label:
        inheritedLead && parent
          ? `No lead (inherits ${inheritedLead} from ${parent.data.name})`
          : "No lead",
    },
    ...[...labels].map(([handle, label]) => ({ value: handle, label })),
    ...(initial.lead !== "" && !labels.has(initial.lead)
      ? [{ value: initial.lead, label: `@${initial.lead} (names no seat)` }]
      : []),
  ];

  const keyProblem = minted
    ? identityProblemOf(takenIdentities(state, api.identities), "unit", form.id, initial.id)
    : null;
  const blocked =
    form.name.trim() === "" ? "A unit needs a name." : keyProblem !== null ? keyProblem : null;

  const parts = unitParts(key, initial, form, { editableKey: minted });
  const credentials = toolCredentialNames(unit.data).length > 0;
  const tabs: EditorTab[] = [
    {
      id: "profile",
      label: "Profile",
      paths: [["name"], ["id"], ["type"], ["purpose"], ["goals"]],
      content: (
        <EditorSection title="Unit">
          <ConfigField
            label="Name"
            value={form.name}
            onChange={(name) => set({ name })}
            disabled={disabled}
            help={NAME_HELP.unit}
            error={errorFor(["name"])}
          />
          {minted ? (
            <ConfigField
              label="Key"
              kind="id"
              value={form.id}
              onChange={(id) => set({ id: id.trim() })}
              disabled={disabled}
              help={IDENTITY_HELP.unit}
              error={keyProblem ?? errorFor(["id"])}
            />
          ) : (
            <ReadOnlyFact label="Key" reason={IDENTITY_FIXED.unit}>
              <InlineCode>{unitIdOf(unit)}</InlineCode>
            </ReadOnlyFact>
          )}
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
        </EditorSection>
      ),
    },
    {
      id: "leadership",
      label: "Leadership",
      paths: [["lead"], ["channel"]],
      content: (
        <EditorSection title="Leadership">
          <div className="builder-pair">
            <ConfigField
              label="Lead"
              kind="choice"
              choices={leadChoices}
              value={form.lead}
              onChange={(lead) => set({ lead })}
              disabled={disabled || leadLimit !== null}
              autoFocus={section === "leadership"}
              help={leadLimit ?? "Sub-units without a lead inherit this one."}
              error={errorFor(["lead"])}
            />
            <ConfigField
              label="Channel"
              value={form.channel}
              onChange={(channel) => set({ channel })}
              required={false}
              disabled={disabled || keyLimit !== null}
              placeholder={inheritedChannel}
              help={
                keyLimit ??
                (inheritedChannel && parent
                  ? `Empty inherits ${inheritedChannel} from ${parent.data.name}.`
                  : "A unit below that names no channel inherits this one.")
              }
              error={errorFor(["channel"])}
            />
          </div>
        </EditorSection>
      ),
    },
    {
      id: "work",
      label: "Work",
      paths: [["project"], ["space"], ["knowledge"], ["schedules"]],
      content: (
        <>
          <Owns
            who="unit"
            focusProject={section === "project"}
            project={form.project}
            space={form.space}
            onProject={(project) => set({ project })}
            onSpace={(space) => set({ space })}
            errorFor={errorFor}
            disabled={disabled}
            keyLimit={keyLimit}
          />
          <EditorSection title="Knowledge">
            <ListInput
              label="References"
              itemName="reference"
              value={form.knowledge}
              onChange={(knowledge) => set({ knowledge })}
              optional
              disabled={disabled}
              helper="Free-text references, not a read scope."
              helperOnFocus
              error={withProblems(errorFor(["knowledge"]))}
            />
          </EditorSection>
          <ScheduleToggles
            data={unit.data}
            unit
            values={form.schedules}
            onChange={(name, enabled) => set({ schedules: { ...form.schedules, [name]: enabled } })}
            disabled={disabled}
            error={errorFor(["schedules"])}
          />
        </>
      ),
    },
    {
      id: "advanced",
      label: "Advanced",
      paths: [["mcp_env"]],
      content: (
        <>
          {credentials && (
            <EditorSection
              title="From the configuration document"
              aside={<ScreenLink to="config">Open the document</ScreenLink>}
            >
              <ToolCredentialFact data={unit.data} />
            </EditorSection>
          )}
          <DeleteNode nodeKey={key} noun="unit" onClose={onClose} />
        </>
      ),
    },
  ];

  return (
    <EditorShell
      title={form.name.trim() || "Unit"}
      subtitle={unitTypeLabel(form.type)}
      icon={<NodeGlyph kind="unit" size="md" />}
      name={unit.data.name || "this unit"}
      dirty={dirty}
      blocked={blocked}
      blockedPath={form.name.trim() === "" ? ["name"] : ["id"]}
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      placed={placedOn(api, key)}
      changed={changedPaths(parts)}
      tabs={tabs}
      initialTab={
        section === "leadership" ? "leadership" : section === "project" ? "work" : undefined
      }
      onApply={() => apply(parts)}
      onClose={onClose}
    />
  );
}

// ---------------------------------------------------------------------------
// A seat
// ---------------------------------------------------------------------------

const GITHUB_TIER: Segment[] = ["integrations", "github", "tier"];
const GITHUB_REPOS: Segment[] = ["integrations", "github", "repos"];
const MATTERMOST_CHANNEL: Segment[] = ["integrations", "mattermost", "channel"];
const MATTERMOST_USERNAME: Segment[] = ["integrations", "mattermost", "username"];

/** The field paths a seat's form draws; see [unitFieldPaths] for why it matters. */
function seatFieldPaths(
  company: CompanyDocument,
  data: ConfigRole,
  { human, minted }: { human: boolean; minted: boolean },
): Segment[][] {
  const seatBlock = (tool: "mattermost") =>
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
          ["avatar"] as Segment[],
          ["avatar", "character"] as Segment[],
          ["avatar", "color"] as Segment[],
          ["behavioral_guidelines"] as Segment[],
          ["llm"] as Segment[],
          // The block, for a problem about its shape, and each window, so a
          // refused ceiling is drawn under the box it was typed in.
          ["token_budget"] as Segment[],
          ...BUDGET_WINDOWS.map(({ period }) => ["token_budget", period] as Segment[]),
          ...(schedulesOf(data).length > 0 ? [["schedules"] as Segment[]] : []),
          ...(isConnected(company, "github") ? [GITHUB_TIER, GITHUB_REPOS] : []),
          ...(seatBlock("mattermost") ? [MATTERMOST_CHANNEL, MATTERMOST_USERNAME] : []),
          ...ownsFieldPaths(company),
        ]),
  ];
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
  preset,
  onClose,
}: {
  seat: DraftSeat;
  section: EditorSectionName | undefined;
  preset: EditorPreset | undefined;
  onClose: () => void;
}) {
  const api = useBuilder();
  const { state } = api;
  const key = seat.key;
  const data = seat.data;
  const company = state.draft.company;
  const minted = isMintedKey(key);
  const human = kindOf(data) === "human";
  const handle = handleOf(seat);
  const { initial, form, set, dirty } = useForm<SeatForm>(
    () => seatForm(data, gitLabAccessLevel(company, handle)),
    presetProject(preset),
  );
  const { refusal, apply } = useApply(api, key, onClose);
  const { errorFor, rest } = placeOnFields(
    placedOn(api, key),
    seatFieldPaths(company, data, { human, minted }),
  );
  const disabled = api.readOnly;
  // A LEAD'S DRAFT keeps the keys another system finds a seat by
  // (`model/scope.ts`), on a seat it added as on a saved one.
  const keyLimit = scopeLimit(state.scope, state.draft, key, "key");
  const handleProblem = minted
    ? identityProblemOf(takenIdentities(state, api.identities), "seat", form.handle, initial.handle)
    : null;

  const budgetErrors = human ? {} : tokenBudgetErrors(form.tokenBudget);
  const blocked =
    form.name.trim() === ""
      ? "A seat needs a name."
      : handleProblem !== null
        ? handleProblem
        : Object.keys(budgetErrors).length > 0
          ? "Correct the token budget first."
          : null;

  const parts = seatParts(key, data, initial, form, { editableHandle: minted });
  const shownHandle = minted ? form.handle.trim() || handle : handle;
  const subtitle = (
    <>
      {shownHandle ? <span className="mono">{handleLabel(shownHandle)}</span> : null}
      {shownHandle ? " · " : null}
      {human ? "Person" : "Agent"}
    </>
  );

  const identity = (
    <EditorSection title="Identity">
      <div className="builder-pair">
        <ConfigField
          label="Name"
          value={form.name}
          onChange={(name) => set({ name })}
          disabled={disabled}
          help={NAME_HELP.seat}
          error={errorFor(["name"])}
        />
        <ConfigField
          label="Email"
          kind="email"
          value={form.email}
          onChange={(email) => set({ email })}
          required={false}
          disabled={disabled || keyLimit !== null}
          help={keyLimit ?? undefined}
          error={errorFor(["email"])}
        />
      </div>
      {minted ? (
        <ConfigField
          label="Handle"
          kind="id"
          value={form.handle}
          onChange={(value) => set({ handle: value.trim() })}
          disabled={disabled}
          help={IDENTITY_HELP.seat}
          error={handleProblem ?? errorFor(["handle"])}
        />
      ) : (
        <HandleFact seat={seat} dirty={dirty} onClose={onClose} />
      )}
      <FactList
        items={[
          {
            key: "kind",
            label: "Kind",
            value: human ? "Person" : "Agent",
            reason: "To change it, delete the seat and add a new one.",
          },
        ]}
      />
      {form.avatar && (
        <FormField
          label="Profile icon"
          error={
            errorFor(["avatar", "character"]) ??
            errorFor(["avatar"]) ??
            errorFor(["avatar", "color"])
          }
        >
          <div className="node-editor-icon">
            <AvatarPicker
              name={form.name || "this seat"}
              size="md"
              value={{ character: form.avatar.character, hue: form.avatar.color }}
              onChange={(next) =>
                set({ avatar: { character: next.character as AvatarCharacter, color: next.hue } })
              }
              characters={AVATAR_CHARACTERS}
              disabled={disabled}
            />
            <span>
              {CREWLET_CHARACTER_GEOMETRY[form.avatar.character].name} ·{" "}
              {NODE_HUE_NAMES[form.avatar.color]}
            </span>
          </div>
        </FormField>
      )}
    </EditorSection>
  );

  const persona = (
    <EditorSection title={human ? "Role" : "Persona"}>
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
    </EditorSection>
  );

  const reports = (
    <Reports
      seat={seat}
      value={form.manages}
      onChange={(manages) => set({ manages })}
      disabled={disabled}
      error={errorFor(["manages"])}
      autoFocus={section === "reports"}
    />
  );

  const remove = <DeleteNode nodeKey={key} noun="seat" onClose={onClose} />;

  const tabs: EditorTab[] = human
    ? [
        {
          id: "profile",
          label: "Profile",
          paths: [["name"], ["handle"], ["email"], ["goal"], ["backstory"], ["responsibilities"]],
          content: (
            <>
              {identity}
              <SeatHolder seat={{ handle, name: data.name }} minted={minted} />
              {persona}
            </>
          ),
        },
        {
          id: "contact",
          label: "Contact",
          paths: [["contact"], ["availability"]],
          content: (
            <>
              {/* OPTIONAL, AND SAID SO: a person who works only through the
                  dashboard has no chat account to declare, and the engine
                  warns rather than refuses a human seat with none. */}
              <EditorSection
                title="Contact identities"
                hint={`How agents mention and reach the person, and how their activity is attributed. Optional: with none, the person is reached through the dashboard only.${keyLimit ? ` ${keyLimit}` : ""}`}
              >
                {errorFor(["contact"]) && (
                  <Callout variant="danger" role="alert">
                    {errorFor(["contact"])}
                  </Callout>
                )}
                <div className="builder-pair">
                  {CONTACT_IDENTITIES.map(({ key: identity, label }) => (
                    <ConfigField
                      key={identity}
                      label={label}
                      mark={<VendorMark vendor={CONTACT_VENDOR[identity]} size="sm" />}
                      kind="id"
                      value={form.contact[identity]}
                      onChange={(value) => set({ contact: { ...form.contact, [identity]: value } })}
                      required={false}
                      disabled={disabled || keyLimit !== null}
                    />
                  ))}
                </div>
              </EditorSection>
              <EditorSection title="Availability">
                <ConfigField
                  label="Working hours"
                  value={form.availability}
                  onChange={(availability) => set({ availability })}
                  required={false}
                  disabled={disabled}
                  help="A free-text note, such as working hours."
                  error={errorFor(["availability"])}
                />
              </EditorSection>
            </>
          ),
        },
        {
          id: "work",
          label: "Work",
          paths: [["manages"]],
          content: (
            <>
              {reports}
              {remove}
            </>
          ),
        },
      ]
    : [
        {
          id: "profile",
          label: "Profile",
          paths: [
            ["name"],
            ["handle"],
            ["email"],
            ["avatar"],
            ["goal"],
            ["backstory"],
            ["responsibilities"],
            ["behavioral_guidelines"],
          ],
          content: (
            <>
              {identity}
              {persona}
            </>
          ),
        },
        {
          id: "work",
          label: "Work",
          paths: [["manages"], ["project"], ["space"], ["schedules"]],
          content: (
            <>
              {reports}
              {/* AN AGENT SEAT'S ONLY: the engine refuses a human seat a
                  project or a space (`org.Role.humanForbidden`), since nothing
                  files work for a person by them. */}
              <Owns
                who="seat"
                focusProject={section === "project"}
                project={form.project}
                space={form.space}
                onProject={(project) => set({ project })}
                onSpace={(space) => set({ space })}
                errorFor={errorFor}
                disabled={disabled}
                keyLimit={keyLimit}
              />
              <ScheduleToggles
                data={data}
                unit={false}
                values={form.schedules}
                onChange={(name, enabled) =>
                  set({ schedules: { ...form.schedules, [name]: enabled } })
                }
                disabled={disabled}
                error={errorFor(["schedules"])}
              />
            </>
          ),
        },
        {
          id: "model",
          label: "Model",
          paths: [["llm"], ["token_budget"], ...PHASE_MODEL_FIELDS.map((f) => [f] as Segment[])],
          content: (
            <ModelSection
              data={data}
              chain={form.llm}
              onChain={(llm) => set({ llm })}
              budget={form.tokenBudget}
              onBudget={(window, typed) =>
                set({ tokenBudget: { ...form.tokenBudget, [window]: typed } })
              }
              budgetError={(window) =>
                budgetErrors[window] ??
                errorFor(["token_budget", window]) ??
                (window === BUDGET_WINDOWS[0].period ? errorFor(["token_budget"]) : undefined)
              }
              chainError={errorFor(["llm"])}
              disabled={disabled}
            />
          ),
        },
        {
          id: "integrations",
          label: "Integrations",
          paths: [["integrations"], ACCESS_LEVEL],
          content: (
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
          ),
        },
        {
          id: "advanced",
          label: "Advanced",
          paths: [["sandbox"], ["workers"], ["placement"], ["learning_enabled"], ["mcp_env"]],
          content: (
            <>
              <DocumentFacts data={data} handle={handle} />
              {remove}
            </>
          ),
        },
      ];

  return (
    <EditorShell
      title={form.name.trim() || (human ? "Person" : "Agent")}
      subtitle={subtitle}
      icon={
        form.avatar ? (
          <AvatarPicker
            name={form.name || "this seat"}
            value={{ character: form.avatar.character, hue: form.avatar.color }}
            onChange={(next) =>
              set({ avatar: { character: next.character as AvatarCharacter, color: next.hue } })
            }
            characters={AVATAR_CHARACTERS}
            disabled={disabled}
          />
        ) : (
          <NodeGlyph kind={human ? "human" : "agent"} size="md" />
        )
      }
      name={data.name || "this seat"}
      dirty={dirty}
      blocked={blocked}
      blockedPath={
        form.name.trim() === "" ? ["name"] : handleProblem !== null ? ["handle"] : ["token_budget"]
      }
      readOnly={api.readOnly}
      refusal={refusal}
      problems={rest}
      placed={placedOn(api, key)}
      changed={changedPaths(parts)}
      tabs={tabs}
      initialTab={section === "reports" || section === "project" ? "work" : undefined}
      onApply={() => apply(parts)}
      onClose={onClose}
    />
  );
}

/**
 * A saved seat's handle, which never changes, and the one way to put the role
 * under another: REPLACING the seat (`model/operations.ts`'s `ReplaceSeat`).
 *
 * A SEAT WITH ANOTHER HANDLE IS ANOTHER SEAT — a new mailbox, new memory and a
 * new agent id — so this says so before it records anything, and the old
 * seat's work stays with the old handle. It is its own step, like a deletion,
 * because it replaces the node this form is editing.
 */
function HandleFact({
  seat,
  dirty,
  onClose,
}: {
  seat: DraftSeat;
  dirty: boolean;
  onClose: () => void;
}) {
  const api = useBuilder();
  const { state } = api;
  const handle = handleOf(seat);
  const [open, setOpen] = useState(false);
  const [typed, setTyped] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const taken = takenIdentities(state, api.identities);
  const next = typed ?? mintHandle(seat.data.name, taken);
  const problem = identityProblemOf(taken, "seat", next, "");
  const uncarried = withoutMasked(seat.data).masked;

  function replace() {
    if (problem !== null || api.readOnly) return;
    const intent = {
      type: "replaceSeat",
      target: seat.key,
      key: mintKey(api.keys),
      handle: next,
    } as const;
    const answer = recordIntent(state, intent);
    if (!answer.ok) {
      setRefusal(answer.message);
      return;
    }
    api.dispatch({ type: "record", intent });
    onClose();
  }

  return (
    <>
      <ReadOnlyFact
        label="Handle"
        reason={
          dirty
            ? `${IDENTITY_FIXED.seat} Apply or discard the changes in this form before replacing it.`
            : IDENTITY_FIXED.seat
        }
      >
        <span className="row wrap">
          <InlineCode>{handle}</InlineCode>
          <Button
            variant="secondary"
            size="small"
            disabled={dirty || api.readOnly}
            aria-expanded={open}
            onClick={() => setOpen(!open)}
          >
            Replace this seat…
          </Button>
        </span>
      </ReadOnlyFact>
      {open && (
        <EditorSection
          title={`Replace ${handleLabel(handle)}`}
          hint={`This is a new seat: a new mailbox, new memory and a new agent id. ${seat.data.name || "This seat"}'s work, memory and history stay with ${handleLabel(handle)}, which stops running once the change is saved. The new seat takes its place and fields, and every lead, manages entry and Datadog fallback naming ${handleLabel(handle)} moves to it.`}
        >
          <Refusal message={refusal} />
          {uncarried.length > 0 && (
            <Callout variant="warning">
              These credentials do not carry over: {uncarried.join(", ")}. {handleLabel(handle)}{" "}
              holds them as values the dashboard never shows, so the new seat starts without them. A{" "}
              <InlineCode>${"{NAME}"}</InlineCode> reference carries over as written.
            </Callout>
          )}
          <ConfigField
            label="New handle"
            kind="id"
            value={next}
            onChange={(value) => {
              setTyped(value.trim());
              setRefusal(null);
            }}
            help={IDENTITY_HELP.seat}
            error={problem ?? undefined}
          />
          <div className="row">
            <span className="spacer" />
            <Button variant="ghost" onClick={() => setOpen(false)}>
              Keep this seat
            </Button>
            <Button variant="danger" disabled={problem !== null} onClick={replace}>
              Replace seat
            </Button>
          </div>
        </EditorSection>
      )}
    </>
  );
}

/**
 * Who holds a human seat, and the gestures that fill or free it: the one
 * [SeatHolding] the seat's page, its peek and its card on the chart draw
 * (`components/people.tsx`), for every signed-in reader — the directory
 * answers each holder's name to everybody and the gestures to an admin — and
 * nothing at all, asked of nobody, before somebody has said who this is.
 *
 * NOT A DRAFT EDIT. Inviting or creating a person, and cancelling the
 * invitation that holds the seat, write the identity directory about the seat
 * the RUNNING company holds, at once and whatever becomes of this draft — so
 * Apply and Undo reach none of them. A seat this draft added holds nobody yet,
 * since the directory knows only the seats a saved revision declares, and says
 * so instead of offering a gesture the engine would refuse.
 */
function SeatHolder({ seat, minted }: { seat: { handle: string; name: string }; minted: boolean }) {
  const viewer = useViewer();
  const reads = !viewer.asking && !viewer.anonymous;
  const seats = useHumanSeats(reads && !minted);
  if (!reads) return null;
  return (
    <EditorSection title="Person">
      {minted ? (
        <p className="builder-note muted">Save the org first.</p>
      ) : (
        <SeatHolding
          seat={seat}
          seats={seats}
          manages={canManagePeople(viewer.grants)}
          held={viewer.grants}
          you={viewer.handle === seat.handle}
        />
      )}
    </EditorSection>
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
  // AN ENTRY IS A HANDLE OR A UNIT KEY, offered by name. One the draft holds
  // nothing of stays on offer as what it is, so it reads as written and the
  // form does not drop it on the next Apply.
  const labels = seatLabels(state.draft);
  const self = handleOf(seat);
  const options: TagsInputOption[] = [];
  for (const [handle, label] of labels) {
    if (handle !== self) options.push({ value: handle, label, group: "Seats" });
  }
  for (const { unit } of allUnits(state.draft)) {
    const key = unitIdOf(unit);
    // A manages entry that names both a seat and a unit names the seat, so a
    // unit keyed like a seat is not offered as a second meaning of it.
    if (labels.has(key)) continue;
    options.push({
      value: key,
      label: unit.data.name || key,
      group: "Units",
      description: "every seat in it",
    });
  }
  for (const entry of value) {
    if (!options.some((o) => o.value === entry)) {
      options.push({ value: entry, label: `@${entry} (names nothing)`, group: "Seats" });
    }
  }

  const derived = derivedSeatOf(state, seat.key);
  const automatic = new Set(derived?.auto_reports ?? []);
  const groups = (currentCheck(state)?.derived.units ?? [])
    .filter((u) => derived?.handle && u.lead === derived.handle)
    .map((u) => ({
      unit: u.name,
      names: (u.seats ?? []).filter((h) => automatic.has(h)).map((h) => nameOfHandle(state, h)),
    }))
    .filter((g) => g.names.length > 0);

  return (
    <EditorSection title="Reporting">
      <FormField
        label="Manages"
        optional
        helper="Seats, or a whole unit."
        helperOnFocus
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
      {groups.length > 0 && (
        <FactList
          items={groups.map((g) => ({
            key: g.unit,
            label: `Lead of ${g.unit}`,
            value: g.names.join(", "),
            reason: "Change the unit’s lead to change this.",
          }))}
        />
      )}
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
  budget: BudgetForm;
  onBudget: (window: BudgetWindow, typed: string) => void;
  budgetError: (window: BudgetWindow) => string | undefined;
  chainError: string | undefined;
  disabled: boolean;
}) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const providers = providerOrder(company);
  const unpinned = unpinnedProvider(company);
  return (
    <>
      <EditorSection title="Model">
        {chain === null ? (
          <ReadOnlyFact
            label="Model"
            reason="This seat chooses a model per phase."
            link={{ to: "config", label: "Edit in the configuration document" }}
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
          <FormField
            label="Providers, in order"
            optional
            helperOnFocus
            helper={
              chain.length > 0
                ? `Tried in this order: ${chain.join(", then ")}.`
                : unpinned
                  ? `Empty runs on ${unpinned}, the default.`
                  : "No model provider yet. Add one in the configuration."
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
        )}
      </EditorSection>
      {/* ONE BOX PER WINDOW, each optional, on one line: a seat may cap its
        day, its week and its month independently, and a turn runs only while
        every capped window has room. The company's own ceilings apply on top. */}
      <EditorSection title="Token ceilings">
        <div className="node-editor-trio">
          {BUDGET_WINDOWS.map(({ period, label, none }) => (
            <ConfigField
              key={period}
              label={label}
              kind="id"
              value={budget[period]}
              onChange={(typed) => onBudget(period, typed)}
              required={false}
              disabled={disabled}
              placeholder={none}
              help={`Tokens per ${period}, like 40M. The company limit also applies.`}
              error={budgetError(period)}
            />
          ))}
        </div>
      </EditorSection>
    </>
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
  handle: string;
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
  // A BOT IS THE ENGINE'S ONLY WHERE ITS TOKEN NAMES A SECRET STORE ENTRY.
  // The provisioner mints into the entry a whole `${VAR}` points at and skips
  // every other seat with a note (`mattermost.PlanFor`), so a literal token,
  // which reaches this screen as its mask, is a bot somebody manages by hand
  // and whose username is theirs to correct.
  const provisioned = isRecord(mattermost) && isWholeReference(mattermost.bot_token);
  const defaultUsername = mattermostBotUsername(company, handle);
  const gitlabDefault = defaultGitLabAccessLevel(company);

  const integrations = useQuery("integrations", undefined, {});
  const tools = integrations.data?.tools;
  const used = (["github", "slack", "mattermost", "gitlab"] as const).filter((tool) =>
    isConnected(company, tool),
  );
  const unused = (["github", "slack", "mattermost", "gitlab"] as const).filter(
    (tool) => !isConnected(company, tool),
  );
  const installed =
    isRecord(github) && github.installation_id !== undefined && github.installation_id !== "";

  return (
    <>
      {used.includes("github") && (
        <ToolCard tool="github" tools={tools}>
          {appSlug && (
            <FactList
              items={[
                {
                  key: "app",
                  label: "This seat's app",
                  value: (
                    <>
                      <InlineCode>{appSlug}</InlineCode>
                      {installed ? "Installed" : "Created, waiting to be installed"}
                    </>
                  ),
                  reason: "Created from Integrations; its credentials are never shown.",
                },
              ]}
            />
          )}
          <div className="builder-pair">
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
              helperOnFocus
              error={withProblems(errorFor(GITHUB_REPOS))}
            />
          </div>
          {enrolling && (
            <Callout variant="info">
              Create its GitHub app in Integrations.{" "}
              <ScreenLink to="integrations">Open Integrations</ScreenLink>
            </Callout>
          )}
          {tierChanged && (
            <Callout variant="warning">Also raise the app’s permissions at GitHub.</Callout>
          )}
        </ToolCard>
      )}

      {used.includes("gitlab") && (
        <ToolCard tool="gitlab" tools={tools}>
          {!hasGitLabProvisioning(company) ? (
            <p className="builder-note muted">
              GitLab isn’t set up. <ScreenLink to="integrations">Open Integrations</ScreenLink>
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
              help={
                minted
                  ? "Follows the seat’s handle."
                  : "The level this seat's GitLab account joins the group with."
              }
            />
          )}
        </ToolCard>
      )}

      {used.includes("slack") && (
        <ToolCard tool="slack" tools={tools}>
          <p className="builder-note muted">
            {isRecord(slack)
              ? "This seat speaks as its own Slack app, in its unit's channel, whose credentials are set from Integrations."
              : "This seat has no Slack app of its own."}{" "}
            <ScreenLink to="integrations">Open Integrations</ScreenLink>
          </p>
        </ToolCard>
      )}

      {used.includes("mattermost") && (
        <ToolCard tool="mattermost" tools={tools}>
          {isRecord(mattermost) ? (
            <div className="builder-pair">
              <ConfigField
                label="Channel"
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
                  reason="Managed by the engine. Changing it would create a second bot."
                >
                  <InlineCode>{form.mattermostUsername || defaultUsername}</InlineCode>
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
                      ? `Managed by hand. Leave empty for ${defaultUsername}.`
                      : "Managed by hand."
                  }
                  error={errorFor(MATTERMOST_USERNAME)}
                />
              )}
            </div>
          ) : (
            <p className="builder-note muted">
              No Mattermost bot for this seat.{" "}
              <ScreenLink to="integrations">Open Integrations</ScreenLink>
            </p>
          )}
        </ToolCard>
      )}

      {/* A TOOL THE COMPANY DOES NOT USE IS ONE LINE, not a section each: its
          mark drained of colour, its name, and the way to connect it. */}
      {unused.length > 0 && (
        <p className="node-editor-unused">
          <span className="node-editor-unused-marks" aria-hidden="true">
            {unused.map((tool) => (
              <VendorMark key={tool} vendor={tool} size="sm" muted />
            ))}
          </span>
          <span>{`${joinNames(unused.map((tool) => TOOL_NAMES[tool]))}: not in use.`}</span>
          <ScreenLink to="integrations">Open Integrations</ScreenLink>
        </p>
      )}
    </>
  );
}

/**
 * A connected tool's card: its own mark, its name, and its company-wide state
 * in the Integrations screen's words and tone. A seat's own detail (its app
 * being created or installed) is a row inside the card, never a second tag.
 */
function ToolCard({
  tool,
  tools,
  children,
}: {
  tool: "github" | "gitlab" | "slack" | "mattermost";
  tools: readonly IntegrationTool[] | undefined;
  children: ReactNode;
}) {
  const rollup = toolRollup(tools, tool);
  return (
    <section className="node-editor-card" aria-label={TOOL_NAMES[tool]}>
      <div className="node-editor-card-head">
        <VendorMark vendor={tool} size="lg" />
        <span className="node-editor-card-name">{TOOL_NAMES[tool]}</span>
        {rollup && (
          <Tag size="xs" variant={TOOL_TONE[rollup.state] ?? "neutral"} appearance="soft">
            {rollup.label}
          </Tag>
        )}
      </div>
      {children}
    </section>
  );
}

/** "A", "A and B", "A, B and C". */
function joinNames(names: readonly string[]): string {
  return names.length <= 1
    ? (names[0] ?? "")
    : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

/**
 * Deleting the node, at the end of its last tab: the way to change a seat's
 * kind is to delete it and add the other kind, so the door is in the editor.
 * It opens the builder's own delete dialog, which says what goes with it.
 */
function DeleteNode({
  nodeKey,
  noun,
  onClose,
}: {
  nodeKey: NodeKey;
  noun: "seat" | "unit";
  onClose: () => void;
}) {
  const api = useBuilder();
  return (
    <EditorSection title="Delete">
      <div className="node-editor-delete">
        <span className="t-caption">Removes it from the draft. Undo works until you save.</span>
        <Button
          variant="danger"
          size="small"
          disabledReason={api.readOnly ? READ_ONLY_REASON : undefined}
          onClick={() => {
            onClose();
            api.openDelete(nodeKey);
          }}
        >
          {noun === "seat" ? "Delete seat" : "Delete unit"}
        </Button>
      </div>
    </EditorSection>
  );
}

/** The mark each contact identity is drawn with. */
const CONTACT_VENDOR: Readonly<Record<HumanContactKey, Vendor>> = {
  slack_user_id: "slack",
  mattermost_user_id: "mattermost",
  atlassian_account_id: "atlassian",
  github_login: "github",
  gitlab_username: "gitlab",
};

/**
 * The seat's settings the builder shows and does not edit: one row each in one
 * list, the reason on hover, and one link to where they are edited.
 */
function DocumentFacts({ data, handle }: { data: ConfigRole; handle: string }) {
  const { state } = useBuilder();
  const company = state.draft.company;
  const facts: FactListItem[] = [];

  const phases = PHASE_MODEL_FIELDS.filter((field) => data[field] !== undefined);
  if (phases.length > 0) {
    facts.push({
      key: "phases",
      label: "Models per phase",
      value: phases
        .map(
          (field) =>
            `${field.replace("llm_", "")}: ${formatPhaseLLM(data[field])
              .map((r) => r.chain)
              .join("; ")}`,
        )
        .join(", "),
      reason: "Set in the configuration document.",
    });
  }
  if (isRecord(data.sandbox)) {
    // THE SEAT PAGE'S WORDS: what the seat's own block sets for the box its
    // executor runs in. Where that box is belongs to the model entry that
    // launches the run, not to this block.
    facts.push({
      key: "sandbox",
      label: "Executor box",
      value: sandboxWords(data),
      reason: "Edit in the configuration file.",
    });
  }
  if (Array.isArray(data.workers) && data.workers.length > 0) {
    facts.push({
      key: "workers",
      label: "Workers",
      value: data.workers.join(", "),
      reason: "Edit in the configuration file.",
    });
  }
  if (isRecord(data.placement)) {
    facts.push({
      key: "placement",
      label: "Placement",
      value: placementSummary(data.placement),
      reason: "Edit in the configuration file.",
    });
  }
  if (typeof data.learning_enabled === "boolean") {
    facts.push({
      key: "learning",
      label: "Learning",
      value: data.learning_enabled ? "On" : "Off",
      reason: "Set in the configuration document.",
    });
  }
  const credentials = toolCredentialNames(data);
  if (credentials.length > 0) facts.push(toolCredentialItem(credentials));
  if (isConnected(company, "datadog")) {
    // A block that is switched off wakes nobody, whatever its route_to says,
    // which is how the engine reads it and how the chart draws it.
    const on = datadogEnabled(company);
    const fallback = datadogFallback(company) === handle;
    facts.push({
      key: "datadog",
      label: (
        <span className="field-mark">
          <VendorMark vendor="datadog" size="sm" />
          Datadog fallback
        </span>
      ),
      value: !on ? "Datadog is off" : fallback ? "This seat" : "Another seat",
      reason: "Set it in Integrations.",
    });
  }
  if (facts.length === 0) return null;
  return (
    <EditorSection
      title="From the configuration document"
      aside={<ScreenLink to="config">Open the document</ScreenLink>}
    >
      <FactList items={facts} />
    </EditorSection>
  );
}
