/**
 * Moving a seat or a unit to another unit, or to the company's top level.
 *
 * A MOVE CHANGES MORE THAN WHERE A CARD SITS, and the engine derives all of
 * it: who a seat reports to (a unit's lead manages its direct members), what
 * a moved unit inherits (a lead and a channel it does not declare), which
 * agent seats onboard again (the unit names above them changed) and which
 * tool credentials a seat receives from its home unit. The dialog previews
 * those from what the check of the draft as it stands reported at both ends
 * (`movePreview.ts`), and the check after the move, and the review before the
 * save, show the engine's own result.
 *
 * A LEAD STAYS A LEAD. A unit's lead is a seat NAME, not a position, so a
 * seat that leads a unit keeps leading it from anywhere in the chart. That is
 * right for a department lead who sits in one of its teams and wrong for a
 * seat moved to another team, so the dialog says "Stays lead of" and offers
 * to clear the lead as part of the move.
 *
 * The move's preconditions are recorded and applied by the model; the dialog
 * previews the very operation it will dispatch (`preflight.ts`), which is how
 * it names the schedules the move would leave with no runner.
 */

import { useState } from "react";
import { plural } from "~/lib/format.ts";
import { ConfigField, type FieldChoice } from "~/components/ConfigField.tsx";
import { useBuilder } from "./BuilderContext.tsx";
import {
  EditorSection,
  ReadOnlyNote,
  Refusal,
  StrandedNotes,
  WorkingNotes,
} from "./dialogParts.tsx";
import { allSeats, allUnits, locate, siblingsAt, subtreeKeys } from "./model/draft.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import type { Intent } from "./model/operations.ts";
import { handlesOf, recordIntent } from "./model/reducer.ts";
import { movePreview, type MovePreview } from "./movePreview.ts";
import { isWorking, unitsLedBy } from "./nodeFacts.ts";
import { newlyStranded, simulate } from "./preflight.ts";
import { MoveItemGlyph } from "@crewlethq/icons/glyphs";
import { Button, Checkbox, Modal } from "@crewlethq/ui";

export function MoveDialog({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const found = locate(state.draft, nodeKey);
  const [destination, setDestination] = useState("");
  const [clearLead, setClearLead] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  if (!found) {
    return (
      <Modal
        open
        stackBody
        title="Move"
        onClose={onClose}
        footer={
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        }
      >
        <p className="t-body">This node is no longer in the draft.</p>
      </Modal>
    );
  }

  const name = found.node.data.name;
  const isUnit = found.kind === "unit";
  const inside = new Set(found.kind === "unit" ? subtreeKeys(found.node) : [nodeKey]);

  // Every unit a node can go to, depth-first and named by its path, so two
  // teams called "Platform" under different departments read apart.
  const parents = new Map([...allUnits(state.draft)].map(({ unit, parent }) => [unit.key, parent]));
  const pathName = (key: NodeKey): string => {
    const unit = locate(state.draft, key);
    const parent = parents.get(key);
    const own = unit?.kind === "unit" ? unit.node.data.name : key;
    return parent === undefined || parent === COMPANY_KEY ? own : `${pathName(parent)} / ${own}`;
  };
  const choices: FieldChoice[] = [
    { value: COMPANY_KEY, label: "The company (top level)" },
    ...[...allUnits(state.draft)]
      .filter(({ unit }) => !inside.has(unit.key))
      .map(({ unit }) => ({ value: unit.key, label: pathName(unit.key) })),
  ];

  const led = isUnit ? [] : unitsLedBy(state.draft, name);
  const unitRef = !isUnit && typeof found.node.data.unit === "string" ? found.node.data.unit : "";

  const intentFor = (to: NodeKey): Intent => {
    const kind = isUnit ? "unit" : "seat";
    const siblings = (siblingsAt(state.draft, to, kind) ?? []).filter((s) => s.key !== nodeKey);
    return {
      type: "move",
      target: nodeKey,
      to: { parent: to, after: siblings.at(-1)?.key ?? null },
      clearLeads: clearLead ? led.map((u) => u.key) : [],
    };
  };

  const chosen = destination !== "";
  const preview = chosen ? simulate(state, intentFor(destination)) : null;
  const derived = chosen ? movePreview(state, nodeKey, destination) : null;
  const stranded = preview?.ok ? newlyStranded(state.draft, preview.after) : [];
  // Already there: its own parent is a place to reorder in, not to move to,
  // unless the seat sits there only by its unit reference, which the move
  // replaces with a placement.
  const sameSpot =
    (chosen && destination === found.parent && unitRef === "") ||
    (preview !== null && !preview.ok && preview.refusal === "no_change");

  const moving = isUnit
    ? [...allSeats(state.draft)].filter(({ parent }) => inside.has(parent)).map(({ seat }) => seat)
    : [found.node];
  const handles = handlesOf(state);
  const working = moving
    .filter((seat) => isWorking(handles.get(seat.key), api.agents, api.sandboxes))
    .map((seat) => seat.data.name);

  function move() {
    if (!chosen || api.readOnly) return;
    const intent = intentFor(destination);
    const answer = recordIntent(state, intent);
    if (!answer.ok) {
      setRefusal(answer.message);
      return;
    }
    api.dispatch({ type: "record", intent });
    onClose();
  }

  const destinationName =
    destination === COMPANY_KEY ? "the top level" : chosen ? pathName(destination) : "";

  return (
    <Modal
      open
      stackBody
      title={`Move ${name}`}
      icon={<MoveItemGlyph />}
      size="md"
      onClose={onClose}
      onSubmit={move}
      footer={
        <>
          <Button variant="tertiary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" disabled={!chosen || sameSpot || api.readOnly}>
            Move
          </Button>
        </>
      }
    >
      {api.readOnly && <ReadOnlyNote />}
      <Refusal message={refusal} />
      <ConfigField
        label="Move to"
        kind="choice"
        choices={choices}
        value={destination}
        onChange={(next) => {
          setDestination(next);
          setRefusal(null);
        }}
        help={
          sameSpot
            ? `${name} is already there.`
            : isUnit
              ? "The unit moves with every seat and unit inside it."
              : undefined
        }
      />
      {unitRef && (
        <p className="t-caption">
          {name} is placed in {unitRef} by its unit reference. Moving it writes it into the
          destination and removes the reference.
        </p>
      )}
      {led.length > 0 && (
        <EditorSection title={`Stays lead of ${led.map((u) => u.data.name).join(", ")}`}>
          <p className="t-caption">
            A unit's lead is a seat's name, so {name} leads {led.length === 1 ? "it" : "them"} from
            wherever it sits.
          </p>
          <Checkbox
            label="Clear lead"
            description={`Removes ${name} as the lead of ${led.map((u) => u.data.name).join(", ")}.`}
            checked={clearLead}
            onCheckedChange={setClearLead}
          />
        </EditorSection>
      )}
      {chosen && !sameSpot && derived && (
        <MoveChanges preview={derived} name={name} destination={destinationName} isUnit={isUnit} />
      )}
      <StrandedNotes stranded={stranded} />
      <WorkingNotes names={working} />
    </Modal>
  );
}

function MoveChanges({
  preview,
  name,
  destination,
  isUnit,
}: {
  preview: MovePreview;
  name: string;
  destination: string;
  isUnit: boolean;
}) {
  if (!preview.known) {
    return (
      <EditorSection title="What changes">
        <p className="t-body muted">
          The engine has not described this draft yet, so what the move changes is shown after the
          next check.
        </p>
      </EditorSection>
    );
  }
  const lines: string[] = [];
  if (!isUnit) {
    // BEFORE THE MOVE, SAID AS SUCH. What the engine will derive after the
    // move is the next check's to report; what is known now is who manages
    // the seat today, and whether that is only as the lead of the unit the
    // seat is leaving, which the move ends.
    lines.push(
      !preview.reportsTo
        ? `Today ${name} reports to nobody.`
        : preview.endsAsLeadOf
          ? `Today ${name} reports to ${preview.reportsTo} as the lead of ${preview.endsAsLeadOf}, which ends with the move.`
          : `Today ${name} reports to ${preview.reportsTo}.`,
    );
    if (preview.destinationLead) {
      lines.push(
        `In ${destination}, ${preview.destinationLead} leads the unit and manages its direct members unless another member manages ${name}.`,
      );
    } else if (destination === "the top level") {
      lines.push("At the top level, no unit lead manages it.");
    }
  }
  for (const lead of preview.leads) {
    lines.push(
      `${lead.unit} would inherit ${lead.after ? `${lead.after} as its lead` : "no lead"} instead of ${lead.before || "none"}.`,
    );
  }
  for (const channel of preview.channels) {
    lines.push(
      `${channel.unit} would inherit ${channel.after ? `the channel ${channel.after}` : "no channel"} instead of ${channel.before || "none"}.`,
    );
  }
  if (preview.onboarding.length > 0) {
    const one = preview.onboarding.length === 1;
    lines.push(
      `${plural(preview.onboarding.length, "agent seat")} ${one ? "onboards" : "onboard"} again, because the units above ${one ? "it" : "them"} change: ${preview.onboarding.join(", ")}.`,
    );
  }
  if (preview.credentials.gained.length > 0) {
    lines.push(`${name} gains the tool credentials of ${preview.credentials.gained.join(", ")}.`);
  }
  if (preview.credentials.lost.length > 0) {
    lines.push(`${name} loses the tool credentials of ${preview.credentials.lost.join(", ")}.`);
  }
  return (
    <EditorSection
      title="What changes"
      hint="From the engine's check of the draft as it stands. The check after the move confirms it, and the review lists it before you save."
    >
      <ul className="builder-list">
        {lines.map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ul>
    </EditorSection>
  );
}
