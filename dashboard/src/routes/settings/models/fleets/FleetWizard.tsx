/**
 * Create a fleet, or edit one from its Settings, step by step as Add model is
 * made (`SetupWizard`): Members (the first one sets the class, and the name
 * follows it until it is typed), Utilization (Balanced, or Sequential with its
 * order) and Review. The steps live in the address (`?fleet=new&step=…`).
 *
 * CREATING A FLEET MOVES NO SEAT: seats and helpers are put on it under Who
 * runs on which model.
 */

import { useState } from "react";
import { readOnlyFrom, useAssistantDraft, useDraftState } from "~/app/assistant/draft.ts";
import {
  Button,
  Callout,
  Checkbox,
  ChoiceCards,
  DescriptionList,
  EmptyState,
  FormField,
  Input,
  List,
  ReorderHandle,
  SetupWizard,
  Text,
  UtilizationDiagram,
  VisuallyHidden,
  useReorder,
  useToast,
  type StepperStep,
} from "@crewlethq/ui";
import { LayersGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { listed } from "~/lib/format.ts";
import type { Fleet, FleetStrategy } from "~/protocol/fleets.ts";
import type { ModelsData, ModelView } from "../data.ts";
import { notSaved } from "../flows/SeatMove.tsx";
import { modelMark } from "../modelPicker.tsx";
import {
  KIND_WORDS,
  STRATEGY_WORDS,
  classOf,
  keyFromName,
  kindOf,
  misfitOf,
  nameProblem,
  suggestedName,
  createFleet,
  saveFleet,
} from "./data.ts";

type Step = "members" | "utilization" | "review";

/** What the flow holds until it saves: the typed name (null while it follows the class), the members in order and the strategy. */
interface FleetDraft {
  name: string | null;
  members: string[];
  strategy: FleetStrategy;
}
const STEPS: readonly Step[] = ["members", "utilization", "review"];
const LABELS: Record<Step, string> = {
  members: "Members",
  utilization: "Utilization",
  review: "Review",
};
const HEADINGS: Record<Step, string> = {
  members: "Members",
  utilization: "Utilization strategy",
  review: "Review",
};
const LEADS: Record<Step, string> = {
  members:
    "The models this fleet spreads work across, all of one model class. Optional: add them later from its Settings.",
  utilization: "How new work is spread across the members.",
  review: "What this creates. Put seats on it under Who runs on which model.",
};

export function FleetWizard({ data, editing }: { data: ModelsData; editing: Fleet | null }) {
  const nav = useNavigator();
  const toast = useToast();
  const [stepParam, setStep] = useParam("step", "members");
  const step: Step = (STEPS as readonly string[]).includes(stepParam)
    ? (stepParam as Step)
    : "members";
  const [draft, setDraft] = useDraftState<FleetDraft>(() => ({
    name: editing ? editing.name : null,
    members: editing ? editing.members.map((m) => m.entry) : [],
    strategy: editing?.strategy ?? "balanced",
  }));
  const { members, strategy, name: typedName } = draft;
  const setMembers = (next: string[]) => setDraft((d) => ({ ...d, members: next }));
  const setTypedName = (next: string) => setDraft((d) => ({ ...d, name: next }));
  const setStrategy = (next: FleetStrategy) => setDraft((d) => ({ ...d, strategy: next }));
  const [tried, setTried] = useState(false);
  const [reached, setReached] = useState<Step>(editing ? "review" : step);
  const [saving, setSaving] = useState(false);
  const [refused, setRefused] = useState("");
  const byKey = new Map(data.models.map((m) => [m.key, m]));
  const first = members[0] ? (byKey.get(members[0]) ?? null) : null;
  const name =
    typedName ??
    (first
      ? suggestedName(classOf(first.model), editing?.key ?? null, data.fleets?.fleets ?? [])
      : "");
  const problem = nameProblem(name, editing?.key ?? null, data.fleets?.fleets ?? []);
  const index = STEPS.indexOf(step);
  // WHAT IS BEING SET UP reaches Ask Crewlet, read-only: the flow's own steps change it.
  useAssistantDraft<FleetDraft>({
    kind: "client",
    id: editing?.key ?? null,
    step,
    help: "This is a fleet being set up in the fleet flow, not saved. Read it to answer questions about it; the person changes it on the flow's own steps.",
    scope: editing
      ? `The changes to the fleet ${editing.name} (not saved)`
      : "The fleet you are creating (not saved)",
    value: draft,
    fields: [
      { path: "name", label: "Fleet name", read: () => name },
      { path: "members", label: "Members", read: (d) => d.members },
      {
        path: "strategy",
        label: "Utilization strategy",
        read: (d) => STRATEGY_WORDS[d.strategy].name,
      },
    ],
    write: readOnlyFrom(setDraft),
  });

  const go = (to: Step) => {
    setStep(to);
    if (STEPS.indexOf(to) > STEPS.indexOf(reached)) setReached(to);
  };
  // BACK TO MODELS & FLEETS AS A NEW ENTRY, which the router starts at the top.
  const close = () => nav.to(["settings", "models"]);
  async function finish() {
    if (problem) {
      setTried(true);
      go("members");
      return;
    }
    // FROM A STEP IT OPENS REVIEW; ON REVIEW IT SAVES.
    if (step !== "review") {
      go("review");
      return;
    }
    const fleetName = name.trim();
    const key =
      editing?.key ??
      keyFromName(fleetName, new Set((data.fleets?.fleets ?? []).map((f) => f.key)));
    setSaving(true);
    setRefused("");
    const outcome = editing
      ? await saveFleet(
          key,
          { name: fleetName, strategy, members, threshold_points: editing.threshold_points },
          `Edit the ${fleetName} fleet`,
        )
      : await createFleet(
          key,
          { name: fleetName, strategy, members },
          `Create the ${fleetName} fleet`,
        );
    setSaving(false);
    if (outcome.kind !== "saved" && outcome.kind !== "unchanged") {
      setRefused(notSaved(outcome, `${fleetName} may or may not have been saved`));
      return;
    }
    data.reload();
    toast.show({
      variant: "success",
      message: `${fleetName} fleet ${editing ? "saved" : "created"}.`,
    });
    close();
  }

  const stepList: StepperStep[] = STEPS.map((id) => ({
    id,
    label: LABELS[id],
    ...(tried && problem && id === "members" && id !== step
      ? { status: "attention" as const }
      : {}),
  }));
  const reviewing = step === "review";
  const saveButton = (variant: "primary" | "secondary") => (
    <Button variant={variant} loading={saving} onClick={() => void finish()}>
      {editing ? "Review and save" : "Review and create"}
    </Button>
  );

  return (
    <SetupWizard
      title={editing ? `Edit ${editing.name}` : "Create a fleet"}
      footerStart={
        <Button variant="ghost" onClick={close}>
          Cancel
        </Button>
      }
      steps={stepList}
      current={step}
      reached={reached}
      onSelect={(id) => go(id as Step)}
      stepsLabel={editing ? `Steps to edit ${editing.name}` : "Steps to create a fleet"}
      heading={HEADINGS[step]}
      lead={editing && reviewing ? "What this saves. Seats on it stay on it." : LEADS[step]}
      wide={reviewing}
      footer={
        <>
          {/* BACK IS ALWAYS IN THE ROW, disabled on the first step, so the row keeps one shape. */}
          <Button
            variant="secondary"
            disabled={index === 0}
            onClick={() => index > 0 && go(STEPS[index - 1]!)}
          >
            Back
          </Button>
          {!reviewing && !problem && (editing || step === "members")
            ? saveButton("secondary")
            : null}
        </>
      }
      primaryAction={
        reviewing ? (
          saveButton("primary")
        ) : (
          <Button
            variant="primary"
            onClick={() => {
              if (step === "members" && problem) {
                setTried(true);
                return;
              }
              go(STEPS[index + 1]!);
            }}
          >
            Continue
          </Button>
        )
      }
    >
      {step === "members" ? (
        <>
          <FormField
            label="Fleet name"
            helper="Suggested from the class of its first member."
            error={tried && problem ? problem : undefined}
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                value={name}
                placeholder="Claude Opus"
                autoComplete="off"
                // THE DEFAULT FLEET KEEPS ITS NAME: the engine makes it and finds it by it.
                readOnly={editing?.is_default ?? false}
                onChange={(e) => setTypedName(e.target.value)}
              />
            )}
          </FormField>
          <FormField as="fieldset" label="Members">
            {() =>
              data.models.map((m) => (
                <MemberChoice
                  key={m.key}
                  model={m}
                  first={first}
                  checked={members.includes(m.key)}
                  onChange={(on) =>
                    setMembers(on ? [...members, m.key] : members.filter((k) => k !== m.key))
                  }
                />
              ))
            }
          </FormField>
        </>
      ) : null}
      {step === "utilization" ? (
        <>
          <ChoiceCards
            label="Utilization strategy"
            value={strategy}
            onChange={(v) => setStrategy(v as FleetStrategy)}
            minCardWidth="sm"
            options={(["balanced", "sequential"] as const).map((s) => ({
              value: s,
              label: STRATEGY_WORDS[s].name,
              description: STRATEGY_WORDS[s].line,
              media: <UtilizationDiagram mode={s} lanes={3} />,
            }))}
          />
          {strategy === "sequential" && members.length > 0 ? (
            <OrderList members={members} byKey={byKey} onOrder={setMembers} />
          ) : null}
        </>
      ) : null}
      {reviewing ? (
        <>
          <DescriptionList
            dense
            items={[
              ["Name", name.trim() || "None yet"],
              [
                "Members",
                members.length === 0
                  ? "No members yet; add them from the fleet's Settings."
                  : strategy === "sequential"
                    ? members.map((m, i) => `${i + 1}. ${m}`).join(", ")
                    : listed(members),
              ],
              ["Utilization strategy", STRATEGY_WORDS[strategy].name],
            ]}
          />
          {refused ? (
            <Callout variant="danger" title="Not saved" live="polite">
              {refused}
            </Callout>
          ) : null}
        </>
      ) : null}
    </SetupWizard>
  );
}

/** A model to take into the fleet: held where it runs another class than the first member. */
function MemberChoice({
  model,
  first,
  checked,
  onChange,
}: {
  model: ModelView;
  first: ModelView | null;
  checked: boolean;
  onChange: (on: boolean) => void;
}) {
  const misfit = first && first.key !== model.key ? misfitOf(model, first) : null;
  return (
    <Checkbox
      framed
      checked={checked}
      disabled={!!misfit}
      onCheckedChange={onChange}
      label={
        <span className="models-entry-cell">
          {modelMark(model)}
          <span className="mono">{model.key}</span>
        </span>
      }
      description={misfit ?? `${KIND_WORDS[kindOf(model)]} · ${classOf(model.model)}`}
    />
  );
}

/** A Sequential fleet's order: each member dragged by its handle, or moved with the arrow keys. */
function OrderList({
  members,
  byKey,
  onOrder,
}: {
  members: string[];
  byKey: ReadonlyMap<string, ModelView>;
  onOrder: (order: string[]) => void;
}) {
  const reorder = useReorder({ keys: members, onReorder: onOrder, nameOf: (k: string) => k });
  return (
    <FormField
      as="fieldset"
      label="Order"
      helper="New work fills the first member until it nears its limit, then the next."
    >
      {() => (
        <>
          <VisuallyHidden id={reorder.hintId}>{reorder.hint}</VisuallyHidden>
          <List variant="numbered-divided" density="compact" {...reorder.containerProps}>
            {reorder.order.map((key) => {
              const model = byKey.get(key);
              return (
                <List.Item
                  key={key}
                  {...reorder.itemProps(key)}
                  leading={<ReorderHandle {...reorder.handleProps(key)} />}
                >
                  <span className="mono">{key}</span>
                  {model ? (
                    <Text as="span" variant="caption" tone="tertiary">
                      {` ${KIND_WORDS[kindOf(model)]}`}
                    </Text>
                  ) : null}
                </List.Item>
              );
            })}
          </List>
        </>
      )}
    </FormField>
  );
}

/** The flow the address asks for: `new` creates a fleet; a key edits that fleet. */
export function FleetFlow({ data, fleetKey }: { data: ModelsData; fleetKey: string }) {
  if (fleetKey === "new") return <FleetWizard data={data} editing={null} />;
  const editing = data.fleets?.fleets.find((f) => f.key === fleetKey) ?? null;
  if (!editing)
    return (
      <EmptyState
        icon={<LayersGlyph size="xl" />}
        title={`No fleet called ${fleetKey}`}
        description="It may have been deleted."
        action={
          <a className="t-link" href={href(["settings", "models"])}>
            Every fleet
          </a>
        }
      />
    );
  return <FleetWizard key={fleetKey} data={data} editing={editing} />;
}
