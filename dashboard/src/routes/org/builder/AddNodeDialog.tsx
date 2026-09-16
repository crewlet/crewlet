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
 * A NAME THAT IS FREE FROM THE START. Seat names are unique and so are unit
 * names, because a lead or a `manages` entry names exactly one seat, and a
 * `manages` entry or a unit reference exactly one unit. People routinely want
 * several seats with one role title, and learning the rule from the next check
 * means inventing a second name after the fact. So the name is pre-filled with
 * one not yet taken, and a typed name that is taken offers the next free one
 * ("Software Engineer 2"). That is a convenience rather than a second
 * validator: the engine still decides, and a name the draft already holds is
 * refused by the check like any other.
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
import { useBuilder, type AddKind } from "./BuilderContext.tsx";
import {
  ContactField,
  ReadOnlyNote,
  Refusal,
  UNIQUE_NAME_HELP,
  UnitTypeField,
} from "./dialogParts.tsx";
import { suggestUniqueName } from "./model/document.ts";
import { locate, seatNames, siblingsAt, unitNames } from "./model/draft.ts";
import { COMPANY_KEY, mintKey, type NodeKey } from "./model/keys.ts";
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

/** What the fields draw and what the foot submits: one add, asked in two places. */
interface AddForm {
  kind: AddKind;
  chooseKind: (next: AddKind) => void;
  name: string;
  setName: (next: string) => void;
  type: string;
  setType: (next: string) => void;
  identity: HumanContactKey;
  setIdentity: (next: HumanContactKey) => void;
  contact: string;
  setContact: (next: string) => void;
  clash: boolean;
  suggestion: string;
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
  const [type, setType] = useState("");
  const [identity, setIdentity] = useState<HumanContactKey>("slack_user_id");
  const [contact, setContact] = useState("");
  const [refusal, setRefusal] = useState<string | null>(null);

  const trimmed = name.trim();
  const clash = trimmed !== "" && taken(kind).has(trimmed);
  const missingParent = parentKey !== COMPANY_KEY && parentNode?.kind !== "unit";

  function add() {
    if (trimmed === "" || api.readOnly || missingParent) return;
    const siblings = siblingsAt(state.draft, parentKey, kind === "unit" ? "unit" : "seat") ?? [];
    const placement = { parent: parentKey, after: siblings.at(-1)?.key ?? null };
    const key = mintKey(api.keys);
    let intent: Intent;
    if (kind === "unit") {
      const data: ConfigUnit = { name: trimmed, ...(type.trim() ? { type: type.trim() } : {}) };
      intent = { type: "addUnit", key, placement, data };
    } else {
      const data: ConfigRole = { name: trimmed };
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
      // A default nobody typed over follows the kind; a typed name is kept.
      if (!named) setName(suggestUniqueName(taken(next), DEFAULT_NAMES[next]));
      setRefusal(null);
    },
    name,
    setName: (next) => {
      setName(next);
      setNamed(true);
      setRefusal(null);
    },
    type,
    setType,
    identity,
    setIdentity,
    contact,
    setContact,
    clash,
    suggestion: clash ? suggestUniqueName(taken(kind), trimmed) : "",
    trimmed,
    refusal,
    missingParent,
    readOnly: api.readOnly,
    blocked: trimmed === "" || api.readOnly || missingParent,
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
      <ConfigField
        label="Name"
        value={form.name}
        onChange={form.setName}
        help={UNIQUE_NAME_HELP[form.kind === "unit" ? "unit" : "seat"]}
      />
      {/* BESIDE THE FIELD, NOT INSIDE ITS DESCRIPTION: the way out of a
          collision is a control, and a button inside the text a screen reader
          reads as the field's description is a control nobody is told about. */}
      {form.clash && (
        <p className="row wrap builder-note">
          <span>
            A {noun} named {form.trimmed} already exists.
          </span>
          <Button variant="secondary" size="small" onClick={() => form.setName(form.suggestion)}>
            {`Use ${form.suggestion}`}
          </Button>
        </p>
      )}
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
            A human seat needs one contact identity before the company can be saved. It can be added
            here or later in the seat's editor.
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
