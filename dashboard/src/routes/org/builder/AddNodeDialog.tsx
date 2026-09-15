/**
 * Adding a unit, an agent seat or a human seat under the company or a unit.
 *
 * A NAME THAT IS FREE FROM THE START. Seat names are unique and so are unit
 * names, because a lead or a `manages` entry names exactly one seat, and a
 * `manages` entry or a unit reference exactly one unit. People routinely want several seats with one role title, and
 * learning the rule from the next check means inventing a second name after
 * the fact. So the name is pre-filled with one not yet taken, and a typed
 * name that is taken offers the next free one ("Software Engineer 2"). That
 * is a convenience rather than a second validator: the engine still decides,
 * and a name the draft already holds is refused by the check like any other.
 *
 * THE KEY IS MINTED HERE, in the event handler, never in the reducer (see
 * `model/keys.ts`), from the Builder's one key source, and the new node goes
 * at the end of its parent's list.
 * Nothing is dispatched until the reducer's own recording door has said it
 * will take the operation, so a refusal stays in the dialog.
 */

import { useState } from "react";
import type { ConfigRole, ConfigUnit, HumanContactKey } from "~/protocol/index.ts";
import { Field } from "~/ui/Field.tsx";
import { Segmented } from "~/ui/primitives.tsx";
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
import { Button, Modal } from "@crewlethq/ui";

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

export function AddNodeDialog({
  parent,
  kind: initialKind = "agent",
  onClose,
}: {
  /** A unit's key, or `null` for the company root. */
  parent: NodeKey | null;
  kind?: AddKind;
  onClose: () => void;
}) {
  const api = useBuilder();
  const { state } = api;
  const parentKey = parent ?? COMPANY_KEY;
  const parentNode = parentKey === COMPANY_KEY ? undefined : locate(state.draft, parentKey);
  const where = parentNode?.kind === "unit" ? parentNode.node.data.name : "the company";

  const taken = (kind: AddKind) =>
    new Set(kind === "unit" ? unitNames(state.draft) : seatNames(state.draft));
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
  const suggestion = clash ? suggestUniqueName(taken(kind), trimmed) : "";
  const noun = kind === "unit" ? "unit" : "seat";
  const missingParent = parentKey !== COMPANY_KEY && parentNode?.kind !== "unit";

  function chooseKind(next: AddKind) {
    setKind(next);
    // A default nobody typed over follows the kind; a typed name is kept.
    if (!named) setName(suggestUniqueName(taken(next), DEFAULT_NAMES[next]));
    setRefusal(null);
  }

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

  return (
    <Modal
      open
      stackBody
      title={`Add to ${where}`}
      icon={<AddGlyph />}
      onClose={onClose}
      onSubmit={add}
      footer={
        <>
          <Button variant="tertiary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            type="submit"
            disabled={trimmed === "" || api.readOnly || missingParent}
          >
            {`Add ${KINDS.find((k) => k.value === kind)!.label.toLowerCase()}`}
          </Button>
        </>
      }
    >
      {api.readOnly && <ReadOnlyNote />}
      <Refusal
        message={
          missingParent
            ? "That unit is no longer in the draft, so nothing can be added to it."
            : refusal
        }
      />
      <Segmented<AddKind>
        ariaLabel="What to add"
        semantics="radio"
        value={kind}
        options={KINDS}
        onChange={chooseKind}
      />
      <Field
        label="Name"
        value={name}
        onChange={(next) => {
          setName(next);
          setNamed(true);
          setRefusal(null);
        }}
        autoFocus
        help={UNIQUE_NAME_HELP[kind === "unit" ? "unit" : "seat"]}
      />
      {/* BESIDE THE FIELD, NOT INSIDE ITS DESCRIPTION: the way out of a
          collision is a control, and a button inside the text a screen reader
          reads as the field's description is a control nobody is told about. */}
      {clash && (
        <p className="row wrap builder-note">
          <span>
            A {noun} named {trimmed} already exists.
          </span>
          <Button
            variant="secondary"
            size="small"
            onClick={() => {
              setName(suggestion);
              setNamed(true);
            }}
          >
            {`Use ${suggestion}`}
          </Button>
        </p>
      )}
      {kind === "unit" && <UnitTypeField value={type} onChange={setType} />}
      {kind === "human" && (
        <>
          <ContactField
            identity={identity}
            value={contact}
            onIdentity={setIdentity}
            onValue={setContact}
          />
          <p className="t-caption">
            A human seat needs one contact identity before the company can be saved. It can be added
            here or later in the seat's editor.
          </p>
        </>
      )}
    </Modal>
  );
}
