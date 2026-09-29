/**
 * Adding a unit, an agent seat or a human seat under the company or a unit.
 *
 * TWO SHELLS, ONE FORM. The structure chart draws this IN the chart, in the
 * ghost card of the node that is about to exist ([AddNodeGhostForm], and
 * `CanvasView`): a form that asks for a child's name while saying nothing
 * about where the child goes is a form missing the one thing a chart is for.
 * The table and the reporting chart have no such place to draw it, so they
 * still ask in a dialog ([AddNodeDialog]). What the two ask, refuse and record
 * is one piece of code (`useAddNode`, `AddNodeFields`), because there is one
 * answer to what can be added under a parent and it must not come apart.
 *
 * A NAME AND AN ADDRESS. The name is prose people read, and two seats may
 * share one; the ADDRESS — a seat's handle, a unit's key — is what every
 * reference names the node by and what the chart creates it under, for good:
 * it is the node's identity, which no rename moves and the chart never issues
 * twice. So the form asks for both, and suggests the address from the name
 * (`document.suggestAddress`) until the operator types their own: free in the
 * draft, and never one the saved chart holds, since a removed node's address
 * is refused for ever and a held one is somebody else's. The name is
 * pre-filled with one not yet taken, which is a convenience rather than a rule.
 *
 * THE KEY IS MINTED HERE, in the event handler, never in the reducer (see
 * `model/keys.ts`), from the Builder's one key source, and the new node takes
 * its place among its siblings by its address, which is the only order the
 * chart keeps.
 * Nothing is dispatched until the reducer's own recording door has said it
 * will take the operation, so a refusal stays where the operator is working.
 */

import { useState } from "react";
import type { HumanContactKey } from "~/protocol/index.ts";
import { ConfigField } from "~/components/ConfigField.tsx";
import { useBuilder, type AddKind } from "./BuilderContext.tsx";
import {
  ADDRESS_HELP,
  ContactField,
  NAME_HELP,
  ReadOnlyNote,
  Refusal,
  UnitTypeField,
} from "./dialogParts.tsx";
import { suggestAddress, suggestUniqueName } from "./model/document.ts";
import {
  allSeats,
  allUnits,
  locate,
  seatHandles,
  seatNames,
  unitKeys,
  unitNames,
  type Draft,
  type SeatData,
  type UnitData,
} from "./model/draft.ts";
import { handleProblem, unitKeyProblem } from "./model/problems.ts";
import { COMPANY_KEY, handleOfKey, mintKey, unitKeyOf, type NodeKey } from "./model/keys.ts";
import type { Intent } from "./model/operations.ts";
import { recordIntent } from "./model/reducer.ts";
import { AddGlyph } from "@crewlethq/icons/glyphs";
import { Button, Modal, SegmentedControl } from "@crewlethq/ui";

const KINDS: { value: AddKind; label: string }[] = [
  { value: "unit", label: "Unit" },
  { value: "agent", label: "Agent seat" },
  { value: "human", label: "Human seat" },
];

const DEFAULT_NAMES: Record<AddKind, string> = {
  unit: "New unit",
  agent: "New agent seat",
  human: "New human seat",
};

/**
 * WHERE AN ADD OPENS: on the KIND, in both shells.
 *
 * It is the question the add is asking, and everything else in the form
 * follows from the answer. The name is pre-filled with one that is free FOR
 * THAT KIND and changes when the kind does; the unit type exists only for a
 * unit and the contact identity only for a human seat. A reader who wants
 * exactly what was offered presses the primary control and is done without
 * touching the name at all.
 *
 * SO THE NAME ASKS FOR NOTHING. It carried `autoFocus`, which was a second
 * answer to the same question and the two shells resolved it differently:
 * in the dialog nothing is hidden, so the field took the focus, and in the
 * chart the ghost is not laid out on the tick the form mounts, a hidden
 * element cannot be focused at all, and the request was lost. One set of
 * fields opening on two different controls is worse than either answer, and
 * a request in the markup that nothing can honour is what made that hard to
 * see. Where the chart is the shell, the design system's canvas puts focus on
 * the first control once the card is placed, which is the same one.
 */

/** What either shell is given. */
export interface AddProps {
  /** A unit's key, or `null` for the company root. */
  parent: NodeKey | null;
  kind?: AddKind;
  onClose: () => void;
}

/**
 * Every address a new node may not take: what the draft holds, what the SAVED
 * chart holds — a node this draft removed keeps its address for ever, since
 * the chart never issues a removed one again — and the address every saved
 * node was CREATED under, which the chart keeps as that node's identity
 * however long ago it was renamed away from it (`problems.reservedAddresses`,
 * which a check would otherwise report only after the add). Seats' and units'
 * together, so a `manages:` entry naming the new node never reads as the other
 * kind. A retired address a node merely used to answer to is not here: a
 * creation may take one, which is how the chart lets an alias go.
 */
function takenAddresses(draft: Draft, base: Draft): Set<string> {
  const identities = [
    ...[...allSeats(base)].map(({ seat }) => handleOfKey(seat.key)),
    ...[...allUnits(base)].map(({ unit }) => unitKeyOf(unit.key)),
  ].filter((address): address is string => address !== undefined);
  return new Set([
    ...seatHandles(draft),
    ...unitKeys(draft),
    ...seatHandles(base),
    ...unitKeys(base),
    ...identities,
  ]);
}

/** What the fields draw and what the foot submits: one add, asked in two places. */
interface AddForm {
  kind: AddKind;
  chooseKind: (next: AddKind) => void;
  name: string;
  setName: (next: string) => void;
  address: string;
  setAddress: (next: string) => void;
  /** Why the address cannot be taken, or `null`. */
  addressProblem: string | null;
  /** Whether a contact identity can be given: it lives in the runtime half. */
  runtimeVisible: boolean;
  type: string;
  setType: (next: string) => void;
  identity: HumanContactKey;
  setIdentity: (next: HumanContactKey) => void;
  contact: string;
  setContact: (next: string) => void;
  trimmed: string;
  refusal: string | null;
  /** The reason nothing can be recorded, drawn wherever the form is drawn. */
  missingParent: boolean;
  readOnly: boolean;
  blocked: boolean;
  /** What the primary control says, which names the kind it will add. */
  action: string;
  add: () => void;
}

/**
 * The whole of an add: its answers, what would refuse it, and the one action
 * that records it.
 */
function useAddNode({ parent, kind: initialKind = "agent", onClose }: AddProps): AddForm {
  const api = useBuilder();
  const { state } = api;
  const parentKey = parent ?? COMPANY_KEY;
  const parentNode = parentKey === COMPANY_KEY ? undefined : locate(state.draft, parentKey);

  const taken = (of: AddKind) =>
    new Set(of === "unit" ? unitNames(state.draft) : seatNames(state.draft));
  const [kind, setKind] = useState<AddKind>(initialKind);
  const [name, setName] = useState(() =>
    suggestUniqueName(taken(initialKind), DEFAULT_NAMES[initialKind]),
  );
  const [named, setNamed] = useState(false);
  const addresses = takenAddresses(state.draft, state.baseDraft);
  const suggest = (forName: string, forKind: AddKind) =>
    suggestAddress(addresses, forName, forKind === "unit" ? "unit" : "seat");
  const [address, setAddress] = useState(() =>
    suggest(suggestUniqueName(taken(initialKind), DEFAULT_NAMES[initialKind]), initialKind),
  );
  const [addressed, setAddressed] = useState(false);
  const [type, setType] = useState("");
  const [identity, setIdentity] = useState<HumanContactKey>("slack_user_id");
  const [contact, setContact] = useState("");
  const [refusal, setRefusal] = useState<string | null>(null);

  const trimmed = name.trim();
  const missingParent = parentKey !== COMPANY_KEY && parentNode?.kind !== "unit";
  const chosenAddress = address.trim();
  const addressProblem =
    (kind === "unit" ? unitKeyProblem(chosenAddress) : handleProblem(chosenAddress)) ??
    (addresses.has(chosenAddress)
      ? `${chosenAddress} is taken: something in this company holds it, or held it.`
      : null);
  const runtimeVisible = state.base.runtimeVisible;

  function add() {
    if (trimmed === "" || api.readOnly || missingParent || addressProblem !== null) return;
    const placement = { parent: parentKey };
    const key = mintKey(api.keys);
    let intent: Intent;
    if (kind === "unit") {
      const data: UnitData = {
        key: chosenAddress,
        name: trimmed,
        ...(type.trim() ? { type: type.trim() } : {}),
      };
      intent = { type: "addUnit", key, placement, data };
    } else {
      const data: SeatData = { handle: chosenAddress, name: trimmed };
      if (kind === "human") {
        data.kind = "human";
        if (runtimeVisible && contact.trim()) {
          data.runtime = { contact: { [identity]: contact.trim() } };
        }
      }
      intent = { type: "addSeat", key, placement, data };
    }
    const answer = recordIntent(state, intent);
    if (!answer.ok) {
      setRefusal(answer.message);
      return;
    }
    api.dispatch({ type: "record", intent });
    onClose();
  }

  return {
    kind,
    chooseKind: (next) => {
      setKind(next);
      // A default nobody typed over follows the kind; a typed one is kept.
      const nextName = named ? name : suggestUniqueName(taken(next), DEFAULT_NAMES[next]);
      if (!named) setName(nextName);
      if (!addressed) setAddress(suggest(nextName, next));
      setRefusal(null);
    },
    name,
    setName: (next) => {
      setName(next);
      setNamed(true);
      // THE ADDRESS FOLLOWS THE NAME until the operator types one of their own.
      if (!addressed) setAddress(suggest(next, kind));
      setRefusal(null);
    },
    address,
    setAddress: (next) => {
      setAddress(next);
      setAddressed(true);
      setRefusal(null);
    },
    addressProblem,
    runtimeVisible,
    type,
    setType,
    identity,
    setIdentity,
    contact,
    setContact,
    trimmed,
    refusal,
    missingParent,
    readOnly: api.readOnly,
    blocked: trimmed === "" || api.readOnly || missingParent || addressProblem !== null,
    action: `Add ${KINDS.find((k) => k.value === kind)!.label.toLowerCase()}`,
    add,
  };
}

/**
 * Everything an add asks, in the order it asks it.
 *
 * DRAWN THE SAME IN BOTH SHELLS. The ghost is a card of the chart rather than
 * a dialog, but what it asks is the same question with the same helps, the
 * same collision suggestion and the same refusals: a form that shed a field or
 * a warning when it moved into the chart would be a second, quieter add.
 */
function AddNodeFields({ form }: { form: AddForm }) {
  const noun = form.kind === "unit" ? "unit" : "seat";
  // A problem with the address is said once the operator has an address to
  // judge: the suggestion is free by construction, so this only ever speaks
  // about one they typed.
  const addressError = form.address.trim() === "" ? undefined : (form.addressProblem ?? undefined);
  return (
    <>
      {form.readOnly && <ReadOnlyNote />}
      <Refusal
        message={
          form.missingParent
            ? "That unit is no longer in the draft, so nothing can be added to it."
            : form.refusal
        }
      />
      <SegmentedControl<AddKind>
        label="What to add"
        semantics="radio"
        value={form.kind}
        options={KINDS}
        onValueChange={form.chooseKind}
      />
      <ConfigField label="Name" value={form.name} onChange={form.setName} help={NAME_HELP[noun]} />
      <ConfigField
        label={noun === "unit" ? "Key" : "Handle"}
        kind="id"
        value={form.address}
        onChange={form.setAddress}
        help={ADDRESS_HELP[noun].created}
        error={addressError}
      />
      {form.kind === "unit" && <UnitTypeField value={form.type} onChange={form.setType} />}
      {form.kind === "human" && form.runtimeVisible && (
        <>
          <ContactField
            identity={form.identity}
            value={form.contact}
            onIdentity={form.setIdentity}
            onValue={form.setContact}
          />
          <p className="t-caption">
            Optional. Without one, no agent can @-mention this person, which is right for somebody
            who works only through the dashboard. It can be added here or later in the seat's
            editor.
          </p>
        </>
      )}
    </>
  );
}

/**
 * The add as the structure chart draws it: the form inside the ghost card of
 * the node about to exist.
 *
 * IT SAYS NOTHING ABOUT WHERE THE NODE GOES, because the chart says it. The
 * dialog below has to carry "Add to Engineering" as a title; here the form
 * hangs off Engineering's own branch, in the rank the new node will land in,
 * with every sibling drawn beside it. The sentence is not lost to a reader who
 * cannot see that: the ghost is a named region (`TreeComposing.label`), so a
 * screen reader is told what it has entered on the way in, and it is said once
 * rather than twice.
 *
 * NO CLOSE CONTROL BESIDE A CANCEL. One way out per job, which is the rule the
 * dialogs of this lens keep; Escape is the other spelling of the same one, and
 * the chart's own canvas performs it.
 */
export function AddNodeGhostForm({ parent, kind, onClose }: AddProps) {
  const form = useAddNode({ parent, kind, onClose });
  return (
    <form
      className="col gap-3"
      onSubmit={(event) => {
        event.preventDefault();
        form.add();
      }}
    >
      <AddNodeFields form={form} />
      {/* FULL-SIZED CONTROLS, unlike the chart this is drawn from, which
          shrinks its inline form to the chart's own 9px type because the form
          is drawn at whatever zoom the reader left the chart at. Here the
          canvas EASES onto the ghost and gives it two thirds of the pane, so
          the form is drawn at about its own size and every pointer target is
          the one the rest of the lens uses. */}
      <div className="row">
        <span className="spacer" />
        <Button variant="tertiary" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="primary" type="submit" disabled={form.blocked}>
          {form.action}
        </Button>
      </div>
    </form>
  );
}

/**
 * The add as a dialog, for the views with no structure chart behind them: the
 * table, and the reporting chart, which draws who reports to whom rather than
 * what is inside what and so has no place for a unit's next child.
 */
export function AddNodeDialog({ parent, kind, onClose }: AddProps) {
  const api = useBuilder();
  const form = useAddNode({ parent, kind, onClose });
  const parentKey = parent ?? COMPANY_KEY;
  const parentNode = parentKey === COMPANY_KEY ? undefined : locate(api.state.draft, parentKey);
  const where = parentNode?.kind === "unit" ? parentNode.node.data.name : "the company";
  return (
    <Modal
      open
      stackBody
      title={`Add to ${where}`}
      icon={<AddGlyph />}
      // ONE WAY OUT PER JOB. Cancel is in the foot; a close control beside the
      // title would be a second, unnamed spelling of it, which is what the
      // console's own dialogs do not have.
      showCloseButton={false}
      onClose={onClose}
      onSubmit={form.add}
      footer={
        <>
          <Button variant="tertiary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" disabled={form.blocked}>
            {form.action}
          </Button>
        </>
      }
    >
      <AddNodeFields form={form} />
    </Modal>
  );
}
