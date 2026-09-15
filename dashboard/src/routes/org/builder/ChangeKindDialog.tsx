/**
 * Changing a seat between an agent seat and a human seat.
 *
 * A KIND CHANGE REMOVES FIELDS, which is why it is its own step rather than a
 * control in the editor. A human seat runs no turns, so the engine refuses it
 * every runtime field (`org.Role.humanForbidden`) and, on admission, its own
 * GitHub App; an agent seat is refused `contact` and `availability`. The
 * operation strips exactly those, and the dialog lists them by their authored
 * names before anything is recorded.
 *
 * A STRIPPED CREDENTIAL DOES NOT COME BACK. The configuration surface sends a
 * literal credential as a mask, so the builder never holds one and cannot type
 * one back in: the seat's chat app tokens, its tool credentials and its GitHub
 * App key are gone from the document once the change is saved. That is called
 * out on the fields that hold them.
 *
 * WHAT THE SEAT BECOMES. A human seat needs one contact identity, which the
 * dialog collects, because the engine refuses a human seat without one and the
 * refusal would otherwise arrive at the next check with no field to fix. A
 * seat that is the Datadog fallback cannot become human (an alert would wake
 * nobody), so a replacement is chosen here too, and the schedules the change
 * strands are named as they are for a move or a removal.
 *
 * WHAT IT LEAVES BEHIND. Stripping a field tears nothing down at a vendor:
 * the seat's GitHub App, its chat bots and the accounts it was enrolled for
 * stay where they are, and so do the secret store entries the removed fields
 * referenced, the same as for a deleted seat. They are named, never by value.
 */

import { useState } from "react";
import type { HumanContactKey } from "~/protocol/index.ts";
import { Field } from "~/ui/Field.tsx";
import { Button } from "~/ui/primitives.tsx";
import { useBuilder } from "./BuilderContext.tsx";
import {
  ContactField,
  EditorSection,
  ReadOnlyNote,
  Refusal,
  ScreenLink,
  StaysUntilDecommissioned,
  StrandedNotes,
  WorkingNotes,
  type LeftBehind,
} from "./dialogParts.tsx";
import { allSeats, locate } from "./model/draft.ts";
import { isMintedKey, type NodeKey } from "./model/keys.ts";
import { fieldName, isCredentialField, kindOf, type Intent } from "./model/operations.ts";
import { handlesOf, recordIntent } from "./model/reducer.ts";
import { datadogFallback } from "./chartModel.ts";
import { isWorking, referenceNames, vendorIdentities } from "./nodeFacts.ts";
import { newlyStranded, simulate } from "./preflight.ts";
import { PersonGlyph, SmartToyGlyph } from "@crewlethq/icons/glyphs";
import { Callout, Modal } from "@crewlethq/ui";

export function ChangeKindDialog({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const [identity, setIdentity] = useState<HumanContactKey>("slack_user_id");
  const [contact, setContact] = useState("");
  const [routeTo, setRouteTo] = useState("");
  const [refusal, setRefusal] = useState<string | null>(null);
  const found = locate(state.draft, nodeKey);

  if (found?.kind !== "seat") {
    return (
      <Modal
        open
        stackBody
        title="Change the kind of seat"
        onClose={onClose}
        footer={<Button onClick={onClose}>Close</Button>}
      >
        <p className="t-body">This seat is no longer in the draft.</p>
      </Modal>
    );
  }

  const seat = found.node;
  const name = seat.data.name;
  const becoming = kindOf(seat.data) === "human" ? "agent" : "human";
  const handles = handlesOf(state);
  const handle = handles.get(nodeKey);
  const isFallback =
    becoming === "human" && handle !== undefined && datadogFallback(state.draft.company) === handle;
  const replacements = [...allSeats(state.draft)]
    .map(({ seat: other }) => other)
    .filter((other) => other.key !== nodeKey && kindOf(other.data) === "agent")
    .map((other) => ({ value: handles.get(other.key) ?? "", label: other.data.name }))
    .filter((choice) => choice.value !== "");

  const intent: Intent = {
    type: "changeKind",
    target: nodeKey,
    kind: becoming,
    ...(becoming === "human" && contact.trim() !== ""
      ? { contact: { [identity]: contact.trim() } }
      : {}),
    ...(isFallback && routeTo !== "" ? { routeTo } : {}),
  };
  const preview = simulate(state, intent);
  const op = preview.ok && preview.op.type === "changeKind" ? preview.op : null;
  const stripped = (op?.stripped ?? []).map((field) => ({
    name: fieldName(field.path),
    credential: isCredentialField(field.path),
  }));
  const stranded = preview.ok ? newlyStranded(state.draft, preview.after) : [];
  const leftBehind: LeftBehind[] =
    becoming === "human" && !isMintedKey(nodeKey)
      ? [
          {
            key: nodeKey,
            name,
            made: vendorIdentities(state, seat),
            references: referenceNames((op?.stripped ?? []).map((field) => field.before)),
          },
        ].filter((entry) => entry.made.length > 0 || entry.references.length > 0)
      : [];
  // Only an agent seat has work in flight, so this names nobody when a human
  // seat is becoming an agent.
  const working = isWorking(handle, api.agents, api.sandboxes) ? [name] : [];

  const blocked =
    !preview.ok ||
    api.readOnly ||
    (becoming === "human" && contact.trim() === "") ||
    (isFallback && routeTo === "");

  function change() {
    if (blocked) return;
    const answer = recordIntent(state, intent);
    if (!answer.ok) {
      setRefusal(answer.message);
      return;
    }
    api.dispatch({ type: "record", intent });
    onClose();
  }

  const title =
    becoming === "human" ? `Change ${name} to a human seat` : `Change ${name} to an agent seat`;

  return (
    <Modal
      open
      stackBody
      title={title}
      icon={becoming === "human" ? <PersonGlyph /> : <SmartToyGlyph />}
      size="md"
      onClose={onClose}
      onSubmit={change}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" disabled={blocked}>
            {becoming === "human" ? "Change to human seat" : "Change to agent seat"}
          </Button>
        </>
      }
    >
      {api.readOnly && <ReadOnlyNote />}
      <Refusal message={refusal ?? (preview.ok ? null : preview.message)} />
      <p className="t-body">
        {becoming === "human"
          ? isMintedKey(nodeKey)
            ? `${name} will not run: a human seat is a person in the chart, with no agent behind it.`
            : `${name} stops running. Its memory is kept but unused while it is a human seat.`
          : `${name} starts running as an agent once the engine applies the change, on the company's model providers.`}
      </p>

      {stripped.length > 0 && (
        <EditorSection
          title="Fields that are removed"
          hint={
            stripped.some((field) => field.credential)
              ? "A field that holds credentials cannot be entered again in the builder, which never shows one, so it is gone for good once this change is saved."
              : undefined
          }
        >
          <ul className="builder-list">
            {stripped.map((field) => (
              <li key={field.name}>
                <code className="inline">{field.name}</code>
                {field.credential && " holds credentials, which are gone for good"}
              </li>
            ))}
          </ul>
        </EditorSection>
      )}

      {leftBehind.length > 0 && (
        <EditorSection title="Outside the chart">
          <StaysUntilDecommissioned entries={leftBehind} />
        </EditorSection>
      )}

      {becoming === "human" && (
        <EditorSection
          title="Contact identity"
          hint="A human seat is reached through a person's own identity, and the engine refuses one without it."
        >
          <ContactField
            identity={identity}
            value={contact}
            onIdentity={setIdentity}
            onValue={setContact}
          />
        </EditorSection>
      )}

      {isFallback &&
        // NO REPLACEMENT IS A DEAD END, AND IT SAYS SO: the change stays
        // unavailable until a fallback is chosen, so with nothing to choose
        // the dialog names the way out rather than leaving a button that never
        // becomes available and a picker with nothing in it.
        (replacements.length === 0 ? (
          <Callout variant="danger">
            {name} is the Datadog fallback, and an alert whose tags name no seat has to wake an
            agent seat. It is the company's only agent seat, so add another before changing this
            one, or disconnect Datadog.{" "}
            <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>
          </Callout>
        ) : (
          <Field
            label="Datadog fallback"
            kind="choice"
            choices={replacements}
            value={routeTo}
            onChange={setRouteTo}
            help={`${name} is the Datadog fallback, and an alert whose tags name no seat has to wake an agent seat. Choose the one that takes over.`}
          />
        ))}

      <StrandedNotes stranded={stranded} />
      <WorkingNotes names={working} />
    </Modal>
  );
}
