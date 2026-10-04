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
 * A NAME IS PROSE AND AN IDENTITY IS CHOSEN HERE. Two seats may share a role
 * title: what a lead, a `manages` entry and a seat's placement name is the
 * seat's HANDLE and the unit's KEY (`model/identity.ts`), which a save fixes
 * for good. So the form asks for both, the identity offered from the name as
 * it is typed — free of every handle and key the draft and the saved company
 * hold, a removed node's included, since a new seat under a removed seat's
 * handle would be that seat again — until the person types one of their own.
 * The identity is held to the engine's grammar before the add is recorded,
 * and the engine still validates the draft it lands in.
 *
 * THE KEY IS MINTED HERE, in the event handler, never in the reducer (see
 * `model/keys.ts`), from the Builder's one key source, and the new node goes
 * at the end of its parent's list.
 * Nothing is dispatched until the reducer's own recording door has said it
 * will take the operation, so a refusal stays where the operator is working.
 */

import { useState } from "react";
import type { ConfigRole, ConfigUnit, HumanContactKey } from "~/protocol/index.ts";
import { ConfigField } from "~/components/ConfigField.tsx";
import { useBuilder, type AddKind, type BuilderApi } from "./BuilderContext.tsx";
import {
  ContactField,
  IDENTITY_HELP,
  NAME_HELP,
  ReadOnlyNote,
  Refusal,
  UnitTypeField,
} from "./dialogParts.tsx";
import { identitiesOf, locate, siblingsAt } from "./model/draft.ts";
import { handleProblem, mintHandle, mintUnitKey, unitKeyProblem } from "./model/identity.ts";
import { COMPANY_KEY, mintKey, type NodeKey } from "./model/keys.ts";
import type { Intent } from "./model/operations.ts";
import { recordIntent } from "./model/reducer.ts";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
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
 * follows from the answer. The name is pre-filled for THAT KIND and changes
 * when the kind does, and so does the handle or key offered from it; the unit
 * type exists only for a unit and the contact identity only for a human seat.
 * A reader who wants exactly what was offered presses the primary control and
 * is done without touching the name at all.
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

/**
 * What an add under a parent is called: "Add to Engineering" under a unit,
 * and the company's own name at the root — "the company" only while it has
 * none.
 *
 * ONE WORDING FOR BOTH SHELLS AND EVERY CONTROL THAT OPENS ONE. The dialog
 * said "Add to the company" over the root while the chart's pill, the ghost
 * that pill opened and the table's row all said "Add to Acme" for the same
 * parent.
 */
export function addLabel(parentName: string): string {
  return `Add to ${parentName || "the company"}`;
}

/** The name of the parent an add hangs from, as [addLabel] says it. */
function parentName(draft: BuilderApi["state"]["draft"], parent: NodeKey | null): string {
  const key = parent ?? COMPANY_KEY;
  if (key === COMPANY_KEY) {
    const name = draft.company.name;
    return typeof name === "string" ? name.trim() : "";
  }
  const found = locate(draft, key);
  // A UNIT NO LONGER IN THE DRAFT is named as one rather than as the company,
  // which is what the root's fallback would otherwise have called it; the
  // form's refusal says what became of it.
  return found?.kind === "unit" ? found.node.data.name : "a removed unit";
}

/** What either shell is given. */
export interface AddProps {
  /** A unit's key, or `null` for the company root. */
  parent: NodeKey | null;
  kind?: AddKind;
  onClose: () => void;
}

/** What the fields draw and what the foot submits: one add, asked in two places. */
interface AddForm {
  kind: AddKind;
  chooseKind: (next: AddKind) => void;
  name: string;
  setName: (next: string) => void;
  type: string;
  setType: (next: string) => void;
  /** The handle (a seat) or key (a unit): offered from the name until typed. */
  address: string;
  setAddress: (next: string) => void;
  /** Why `address` cannot be added, or `null`. */
  addressProblem: string | null;
  identity: HumanContactKey;
  setIdentity: (next: HumanContactKey) => void;
  contact: string;
  setContact: (next: string) => void;
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

  // EVERY IDENTITY THE COMPANY HAS HELD, the saved company's as well as the
  // draft's: a removed seat's handle is still its memory and mailbox.
  const taken = new Set([...identitiesOf(state.draft), ...identitiesOf(state.baseDraft)]);
  const [kind, setKind] = useState<AddKind>(initialKind);
  const [name, setName] = useState(DEFAULT_NAMES[initialKind]);
  const [named, setNamed] = useState(false);
  const [typed, setTyped] = useState<string | null>(null);
  const [type, setType] = useState("");
  const [identity, setIdentity] = useState<HumanContactKey>("slack_user_id");
  const [contact, setContact] = useState("");
  const [refusal, setRefusal] = useState<string | null>(null);

  const trimmed = name.trim();
  const address =
    typed ?? (kind === "unit" ? mintUnitKey(trimmed, taken) : mintHandle(trimmed, taken));
  const addressProblem =
    kind === "unit" ? unitKeyProblem(address, taken) : handleProblem(address, taken);
  const missingParent = parentKey !== COMPANY_KEY && parentNode?.kind !== "unit";
  const blocked = trimmed === "" || addressProblem !== null || api.readOnly || missingParent;

  function add() {
    if (blocked) return;
    const siblings = siblingsAt(state.draft, parentKey, kind === "unit" ? "unit" : "seat") ?? [];
    const placement = { parent: parentKey, after: siblings.at(-1)?.key ?? null };
    const key = mintKey(api.keys);
    let intent: Intent;
    if (kind === "unit") {
      const data: ConfigUnit = {
        name: trimmed,
        id: address,
        ...(type.trim() ? { type: type.trim() } : {}),
      };
      intent = { type: "addUnit", key, placement, data };
    } else {
      const data: ConfigRole = { name: trimmed, handle: address };
      if (kind === "human") {
        data.kind = "human";
        if (contact.trim()) data.contact = { [identity]: contact.trim() };
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
      // A default nobody typed over follows the kind; a typed name is kept,
      // and so is a typed handle unless the kind moved between a seat and a
      // unit, whose identities follow different grammars.
      if (!named) setName(DEFAULT_NAMES[next]);
      if ((next === "unit") !== (kind === "unit")) setTyped(null);
      setRefusal(null);
    },
    name,
    setName: (next) => {
      setName(next);
      setNamed(true);
      setRefusal(null);
    },
    address,
    setAddress: (next) => {
      setTyped(next.trim());
      setRefusal(null);
    },
    addressProblem,
    type,
    setType,
    identity,
    setIdentity,
    contact,
    setContact,
    refusal,
    missingParent,
    readOnly: api.readOnly,
    blocked,
    action: `Add ${KINDS.find((k) => k.value === kind)!.label.toLowerCase()}`,
    add,
  };
}

/**
 * Everything an add asks, in the order it asks it.
 *
 * DRAWN THE SAME IN BOTH SHELLS. The ghost is a card of the chart rather than
 * a dialog, but what it asks is the same question with the same helps and the
 * same refusals: a form that shed a field or a warning when it moved into the
 * chart would be a second, quieter add.
 */
function AddNodeFields({ form }: { form: AddForm }) {
  const noun = form.kind === "unit" ? "unit" : "seat";
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
        help={IDENTITY_HELP[noun]}
        error={form.addressProblem ?? undefined}
      />
      {form.kind === "unit" && <UnitTypeField value={form.type} onChange={form.setType} />}
      {form.kind === "human" && (
        <>
          <ContactField
            identity={form.identity}
            value={form.contact}
            onIdentity={form.setIdentity}
            onValue={form.setContact}
          />
          <p className="t-caption">
            Optional. Without one, no agent can mention this person and they are reached through the
            dashboard only. It can be added here or later in the seat's editor.
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
 * IT SAYS WHERE THE NODE GOES, as the dialog below does, although the chart
 * draws it too. The form hangs off its parent's own branch, in the rank the
 * new node will land in — but the canvas eases onto the ghost, and at the
 * company's root the ghost is a card wide of the parent at the far end of a
 * long branch: arriving from Agents › Add seat, the root and the siblings the
 * node joins were off the canvas and nothing on screen said where it would go.
 *
 * SAID ONCE TO A SCREEN READER. The ghost is a named region with this same
 * sentence (`TreeComposing.label`), announced on the way in, so the line is
 * hidden from assistive technology rather than read a second time.
 *
 * NO CLOSE CONTROL BESIDE A CANCEL. One way out per job, which is the rule the
 * dialogs of this builder keep; Escape is the other spelling of the same one, and
 * the chart's own canvas performs it.
 */
export function AddNodeGhostForm({ parent, kind, onClose }: AddProps) {
  const api = useBuilder();
  const form = useAddNode({ parent, kind, onClose });
  return (
    <form
      className="col gap-3"
      onSubmit={(event) => {
        event.preventDefault();
        form.add();
      }}
    >
      <p className="builder-section-title" aria-hidden="true">
        {addLabel(parentName(api.state.draft, parent))}
      </p>
      <AddNodeFields form={form} />
      {/* FULL-SIZED CONTROLS, unlike the chart this is drawn from, which
          shrinks its inline form to the chart's own 9px type because the form
          is drawn at whatever zoom the reader left the chart at. Here the
          canvas EASES onto the ghost and gives it two thirds of the pane, so
          the form is drawn at about its own size and every pointer target is
          the one the rest of the builder uses. */}
      <div className="row">
        <span className="spacer" />
        <Button variant="ghost" onClick={onClose}>
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
  return (
    <Modal
      open
      stackBody
      title={addLabel(parentName(api.state.draft, parent))}
      icon={<PlusGlyph />}
      // ONE WAY OUT PER JOB. Cancel is in the foot; a close control beside the
      // title would be a second, unnamed spelling of it, which is what the
      // console's own dialogs do not have.
      showCloseButton={false}
      onClose={onClose}
      onSubmit={form.add}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
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
