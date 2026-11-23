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
 * A LEAD STAYS A LEAD. A unit's lead is a seat's HANDLE, not a position, so a
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
import { useAgents, useExecutors } from "~/lib/store-hooks.ts";
import { ConfigField, type FieldChoice } from "~/components/ConfigField.tsx";
import { useBuilder } from "./BuilderContext.tsx";
import { ReadOnlyNote, Refusal, StrandedNotes, WorkingNotes } from "./dialogParts.tsx";
import {
  allSeats,
  allUnits,
  handleOf,
  locate,
  siblingsAt,
  subtreeKeys,
  unitWithKey,
} from "./model/draft.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import type { Intent } from "./model/operations.ts";
import { recordIntent } from "./model/reducer.ts";
import { movePreview, type MovePreview } from "./movePreview.ts";
import { isWorking, unitsLedBy } from "./nodeFacts.ts";
import { newlyStranded, simulate } from "./preflight.ts";
import { FolderInputGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  ChangeGroups,
  ConsequenceList,
  type ChangeGroup,
  type ChangeListRow,
  type ConsequenceFact,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";

export function MoveDialog({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const api = useBuilder();
  const rows = useAgents();
  const executors = useExecutors();
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
        // The one action IS Close; a control beside the title saying the same
        // word twice is the duplicate this pass is removing.
        showCloseButton={false}
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

  const led = found.kind === "unit" ? [] : unitsLedBy(state.draft, handleOf(found.node));
  const ledNames = led.map((u) => u.data.name).join(", ");
  const unitRef = !isUnit && typeof found.node.data.unit === "string" ? found.node.data.unit : "";
  // The reference names the unit's key; a person reads the unit's name.
  const unitRefName = unitWithKey(state.draft, unitRef)?.data.name || unitRef;

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
  const working = moving
    .filter((seat) => isWorking(handleOf(seat), rows, executors))
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
      icon={<FolderInputGlyph />}
      size="md"
      onClose={onClose}
      onSubmit={move}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
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
          {name} is placed in {unitRefName} by reference. Moving it removes the reference.
        </p>
      )}
      {led.length > 0 && (
        <ConsequenceList
          heading={`Stays lead of ${ledNames}`}
          items={[
            {
              kind: "fact",
              key: "stays",
              tone: "kept",
              text: `${name} still leads ${led.length === 1 ? "it" : "them"} after the move: a lead names a seat by its handle.`,
            },
            {
              kind: "choice",
              key: "clear-lead",
              label: "Clear lead",
              description: `Removes ${name} as the lead of ${ledNames}.`,
              checked: clearLead,
              onCheckedChange: setClearLead,
            },
          ]}
        />
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
      <ConsequenceList
        heading="What changes"
        items={[
          {
            kind: "fact",
            key: "later",
            tone: "info",
            text: "Changes appear after the next check.",
          },
        ]}
      />
    );
  }
  const groups = moveChangeGroups(preview, name, destination, isUnit);
  const follows: ConsequenceFact[] = [];
  if (preview.onboarding.length > 0) {
    const one = preview.onboarding.length === 1;
    follows.push({
      kind: "fact",
      key: "onboarding",
      tone: "follows",
      text: `${plural(preview.onboarding.length, "agent seat")} ${one ? "onboards" : "onboard"} again, because the units above ${one ? "it" : "them"} change: ${preview.onboarding.join(", ")}.`,
    });
  }
  return (
    <>
      {groups.length > 0 && <ChangeGroups heading="What changes" groups={groups} />}
      {follows.length > 0 && (
        <ConsequenceList heading={groups.length > 0 ? undefined : "What changes"} items={follows} />
      )}
    </>
  );
}

/**
 * The values a move changes, a subject at a time: the moved seat's manager and
 * tool credentials, and the lead and channel each moved unit inherits.
 */
function moveChangeGroups(
  preview: MovePreview,
  name: string,
  destination: string,
  isUnit: boolean,
): ChangeGroup[] {
  const groups: ChangeGroup[] = [];
  const rows: ChangeListRow[] = [];
  if (!isUnit) {
    // BEFORE THE MOVE, SAID AS SUCH. What the engine derives after it is the
    // next check's to report; only a manager the move ends is struck.
    const today = preview.reportsTo ?? "Nobody";
    const atTop = destination === "the top level";
    if (preview.endsAsLeadOf && preview.reportsTo) {
      rows.push(
        preview.destinationLead
          ? {
              kind: "changed",
              label: "Reports to",
              from: preview.reportsTo,
              to: preview.destinationLead,
              note: "Unless someone else manages them.",
            }
          : {
              kind: "removed",
              label: "Reports to",
              from: preview.reportsTo,
              ...(atTop ? { note: "At the top level, no unit lead manages it." } : {}),
            },
      );
    } else if (preview.destinationLead && !preview.reportsTo) {
      rows.push({
        kind: "added",
        label: "Reports to",
        to: preview.destinationLead,
        note: "Unless someone else manages them.",
      });
    } else {
      const note = preview.destinationLead
        ? `In ${destination}, ${preview.destinationLead} manages them unless someone else does.`
        : atTop
          ? "At the top level, no unit lead manages it."
          : undefined;
      rows.push({
        kind: "fact",
        label: "Reports to today",
        value: today,
        ...(note ? { note } : {}),
      });
    }
  }
  if (preview.credentials.gained.length > 0) {
    rows.push({
      kind: "added",
      label: "Tool credentials",
      to: preview.credentials.gained.join(", "),
    });
  }
  if (preview.credentials.lost.length > 0) {
    rows.push({
      kind: "removed",
      label: "Tool credentials",
      from: preview.credentials.lost.join(", "),
    });
  }
  if (rows.length > 0) groups.push({ key: "moved", subject: name, rows });

  const units = new Map<string, ChangeListRow[]>();
  const inherit = (unit: string, row: ChangeListRow) =>
    units.set(unit, [...(units.get(unit) ?? []), row]);
  for (const lead of preview.leads) {
    inherit(lead.unit, {
      kind: "changed",
      label: "Inherited lead",
      from: lead.before || "None",
      to: lead.after || "None",
    });
  }
  for (const channel of preview.channels) {
    inherit(channel.unit, {
      kind: "changed",
      label: "Inherited channel",
      from: channel.before || "None",
      to: channel.after || "None",
    });
  }
  for (const [unit, unitRows] of units)
    groups.push({ key: `unit:${unit}`, subject: unit, rows: unitRows });
  return groups;
}
