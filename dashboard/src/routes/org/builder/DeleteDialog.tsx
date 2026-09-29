/**
 * Deleting a seat, or a unit with everything inside it.
 *
 * A DELETE REACHES PAST THE CHART, and the dialog says how far before it
 * happens. Inside the chart the model clears what named the removed nodes (a
 * lead, a `manages` entry), and the dialog previews the very operation it will
 * dispatch, so the references it lists are the ones the operation clears.
 * And a removal is the one structural change nothing undoes once it is saved:
 * the chart TOMBSTONES the address, so it is never given to anything again.
 * Outside the chart:
 *
 * - A removed Datadog fallback seat must be replaced, because the engine
 *   refuses a Datadog block whose `route_to` names no agent seat, and
 *   nowhere else in the builder could fix that refusal. So the dialog asks
 *   for the replacement and writes it with the removal.
 * - A removed seat's GitLab access level is removed with it: the entry is
 *   keyed by handle, and left behind it would be a setting about nobody.
 * - What exists for the seat at the vendors (a GitHub App, a Slack app, a
 *   Mattermost bot, the GitLab, Datadog or Atlassian account it is enrolled
 *   for) and the secret store entries it references stay until someone
 *   decommissions them, and the dialog lists them by name
 *   (`vendorIdentities`).
 * - The seat's mailbox is retired 24 hours after the engine applies the
 *   removal, its coding runs are ended then, and its memory is kept under an
 *   identity no later seat can have (`docs/concepts/seat-ownership.md`, "The
 *   removed seat").
 *
 * A removal of more than half the saved company's seats needs its own
 * acknowledgement.
 */

import { useState, type ReactNode } from "react";
import { plural } from "~/lib/format.ts";
import { ConfigField } from "~/components/ConfigField.tsx";
import { useBuilder } from "./BuilderContext.tsx";
import {
  EditorSection,
  NameList,
  ReadOnlyNote,
  Refusal,
  ScreenLink,
  StaysUntilDecommissioned,
  StrandedNotes,
  WorkingNotes,
  type LeftBehind,
} from "./dialogParts.tsx";
import { allSeats, locate, type DraftSeat } from "./model/draft.ts";
import { COMPANY_KEY, isMintedKey, type NodeKey } from "./model/keys.ts";
import { kindOf, type Intent, type ReferenceEffect } from "./model/operations.ts";
import { recordIntent, type BuilderState } from "./model/reducer.ts";
import { datadogFallback } from "./chartModel.ts";
import { isWorking, referenceNames, savedHandleOf, vendorIdentities } from "./nodeFacts.ts";
import { massRemoval, newlyStranded, removedSeats, removedUnits, simulate } from "./preflight.ts";
import { DeleteGlyph } from "@crewlethq/icons/glyphs";
import { Button, Callout, Checkbox, Modal } from "@crewlethq/ui";

export function DeleteDialog({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const [routeTo, setRouteTo] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const found = locate(state.draft, nodeKey);

  if (!found) {
    return (
      <Modal
        open
        stackBody
        title="Delete"
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

  const name =
    found.kind === "seat"
      ? found.node.data.name || found.node.data.handle
      : found.node.data.name || found.node.data.key;
  const company = state.draft.company;

  // What the removal takes, before a replacement fallback is chosen: the
  // replacement cannot be one of the seats it removes.
  const bare = simulate(state, { type: "remove", target: nodeKey });
  const removing = bare.ok ? removedSeats(state.draft, bare.after) : [];
  const fallback = datadogFallback(company);
  const fallbackSeat =
    fallback === undefined ? undefined : removing.find((seat) => seat.data.handle === fallback);
  const replacements = [...allSeats(state.draft)]
    .map(({ seat }) => seat)
    .filter((seat) => kindOf(seat.data) === "agent" && !removing.includes(seat));
  // A CHOICE COUNTS ONLY WHILE IT IS STILL ON OFFER: a choice the replacements
  // no longer hold would write a fallback naming a seat this very removal
  // deletes, which nothing in the builder could fix afterwards.
  const replacement = replacements.some((seat) => seat.data.handle === routeTo) ? routeTo : "";

  const intent: Intent = {
    type: "remove",
    target: nodeKey,
    ...(fallbackSeat && replacement !== "" ? { routeTo: replacement } : {}),
  };
  const preview = simulate(state, intent);
  const op = preview.ok && preview.op.type === "remove" ? preview.op : null;
  const after = preview.ok ? preview.after : state.draft;
  const units = preview.ok ? removedUnits(state.draft, after).filter((u) => u.key !== nodeKey) : [];
  const seats = preview.ok ? removedSeats(state.draft, after) : [];
  const cleared = preview.ok ? preview.report.cleared : [];
  const stranded = preview.ok ? newlyStranded(state.draft, after) : [];
  const mass = preview.ok ? massRemoval(state.baseDraft, after) : null;
  const agents = seats.filter((seat) => kindOf(seat.data) === "agent");
  const working = seats
    .filter((seat) => isWorking(savedHandleOf(state, seat.key), api.agents, api.sandboxes))
    .map((seat) => seat.data.name || seat.data.handle);

  const blocked =
    !preview.ok ||
    api.readOnly ||
    (fallbackSeat !== undefined && replacement === "") ||
    (mass !== null && !acknowledged);

  function remove() {
    if (blocked) return;
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
      title={`Delete ${name}`}
      icon={<DeleteGlyph />}
      size="md"
      /*
       * AN ALERT, NOT A DIALOG. This interrupts to ask something consequential
       * and irreversible once the draft is saved, so a reader's software is
       * asked to announce the whole surface rather than its name alone: the
       * name says what is being deleted and the body says what goes with it.
       *
       * It keeps the framed shape, unlike the discard prompt, and that is not
       * an oversight. The console's delete is one sentence because its model
       * has no references: this one carries what the removal clears, who
       * takes over a Datadog fallback, which vendor identities are left
       * behind, and a mass-removal acknowledgement. Those are sections of a
       * form, and a form with no bands around it is a wall of text.
       */
      role="alertdialog"
      // ONE WAY OUT PER JOB: Cancel is in the foot.
      showCloseButton={false}
      onClose={onClose}
      onSubmit={remove}
      footer={
        <>
          <Button variant="tertiary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="danger" type="submit" disabled={blocked}>
            Delete
          </Button>
        </>
      }
    >
      {api.readOnly && <ReadOnlyNote />}
      <Refusal message={refusal ?? (preview.ok ? null : preview.message)} />
      <p className="t-body">
        {found.kind === "unit"
          ? units.length === 0 && seats.length === 0
            ? `Deletes the unit ${name}, which holds nothing.`
            : `Deletes the unit ${name} with ${[
                units.length > 0 ? plural(units.length, "unit") : "",
                seats.length > 0 ? plural(seats.length, "seat") : "",
              ]
                .filter(Boolean)
                .join(" and ")} inside it.`
          : `Deletes the ${kindOf(found.node.data) === "human" ? "human" : "agent"} seat ${name}.`}{" "}
        Undo brings it back until the draft is saved. Once it is saved nothing brings it back: the
        chart never gives{" "}
        {found.kind === "unit" ? "a removed unit's key" : "a removed seat's handle"} to anything
        again.
      </p>

      <ClearedReferences
        state={state}
        cleared={cleared.filter((c) => c.kind === "lead" || c.kind === "manages")}
      />

      <OutsideTheChart
        state={state}
        agents={agents}
        fallbackSeat={fallbackSeat}
        replacements={replacements.map((seat) => ({
          value: seat.data.handle,
          label: seat.data.name
            ? `${seat.data.name} (@${seat.data.handle})`
            : `@${seat.data.handle}`,
        }))}
        routeTo={replacement}
        onRouteTo={setRouteTo}
        accessLevels={op?.accessLevels ?? []}
      />

      <StrandedNotes stranded={stranded} />
      <WorkingNotes names={working} />

      {mass && (
        <Checkbox
          framed
          tone="danger"
          label={`Delete ${mass.removed} of the ${plural(mass.total, "seat")} the company has`}
          description="More than half of the saved company's seats would go. Confirm this is the change you mean."
          checked={acknowledged}
          onCheckedChange={setAcknowledged}
        />
      )}
    </Modal>
  );
}

/** The names of the nodes a reference effect's holder and target refer to. */
function holderName(state: BuilderState, key: NodeKey): string {
  if (key === COMPANY_KEY) return "The company";
  const found = locate(state.draft, key);
  if (!found) return key;
  return found.kind === "seat"
    ? found.node.data.name || `@${found.node.data.handle}`
    : found.node.data.name || found.node.data.key;
}

function ClearedReferences({
  state,
  cleared,
}: {
  state: BuilderState;
  cleared: readonly ReferenceEffect[];
}) {
  if (cleared.length === 0) return null;
  const sentence = (effect: ReferenceEffect): string => {
    const holder = holderName(state, effect.holder);
    switch (effect.kind) {
      case "lead":
        return `${holder} no longer has @${effect.from} as its lead.`;
      case "manages":
        return `${holder} no longer manages ${effect.from}.`;
      default:
        return `${holder} no longer refers to ${effect.from}.`;
    }
  };
  return (
    <EditorSection title="References that are cleared">
      <NameList names={cleared.map(sentence)} />
    </EditorSection>
  );
}

function OutsideTheChart({
  state,
  agents,
  fallbackSeat,
  replacements,
  routeTo,
  onRouteTo,
  accessLevels,
}: {
  state: BuilderState;
  agents: readonly DraftSeat[];
  fallbackSeat: DraftSeat | undefined;
  replacements: { value: string; label: string }[];
  routeTo: string;
  onRouteTo: (handle: string) => void;
  accessLevels: readonly { handle: string; before?: string }[];
}) {
  // A seat this draft created was never saved: nothing exists for it at a
  // vendor, no entry was written for it, and it has no mailbox.
  const saved = agents.filter((seat) => !isMintedKey(seat.key));
  const identities: LeftBehind[] = saved
    .map((seat) => ({
      key: seat.key,
      name: seat.data.name || `@${seat.data.handle}`,
      made: vendorIdentities(state, seat),
      references: referenceNames(seat.data.runtime),
    }))
    .filter((entry) => entry.made.length > 0 || entry.references.length > 0);
  const parts: ReactNode[] = [];

  if (fallbackSeat) {
    // NO REPLACEMENT IS A DEAD END, AND IT SAYS SO. Delete stays unavailable
    // until a fallback is chosen, so with nothing to choose the dialog has to
    // name the way out rather than leave a button that never becomes
    // available and a picker with nothing in it.
    parts.push(
      replacements.length === 0 ? (
        <Callout key="datadog" variant="danger">
          {fallbackSeat.data.name} is the Datadog fallback, and the engine refuses a Datadog
          fallback that names no agent seat. This removal would leave no agent seat to take it over,
          so add one first, or disconnect Datadog.{" "}
          <ScreenLink to="integrations">Open Integrations</ScreenLink>
        </Callout>
      ) : (
        <ConfigField
          key="datadog"
          label="Datadog fallback"
          kind="choice"
          choices={replacements}
          value={routeTo}
          onChange={onRouteTo}
          help={`${fallbackSeat.data.name} is the Datadog fallback: an alert whose tags name no seat wakes it. Choose the agent seat that takes over; the engine refuses a Datadog fallback that names no agent seat.`}
        />
      ),
    );
  }
  if (accessLevels.length > 0) {
    parts.push(
      <NameList
        key="gitlab"
        names={accessLevels.map(
          (level) =>
            `The GitLab access level for @${level.handle}${level.before ? ` (${level.before})` : ""} is removed with the seat.`,
        )}
      />,
    );
  }
  if (identities.length > 0 || (saved.length > 0 && !state.base.runtimeVisible)) {
    parts.push(
      <StaysUntilDecommissioned
        key="vendors"
        entries={identities}
        runtimeHidden={!state.base.runtimeVisible}
      />,
    );
  }
  if (saved.length > 0) {
    const one = saved.length === 1;
    parts.push(
      <p key="mailbox" className="t-body">
        {one
          ? "Its mailbox, and the mail still addressed to it, is"
          : "Their mailboxes, and the mail still addressed to them, are"}{" "}
        kept for 24 hours after the engine applies the change and then retired, and any coding runs{" "}
        {one ? "it still has are" : "they still have are"} ended then.{" "}
        {one ? "Its memory is" : "Their memory is"} kept under {one ? "an identity" : "identities"}{" "}
        no seat added later can have.
      </p>,
    );
  }
  if (parts.length === 0) return null;
  return <EditorSection title="Outside the chart">{parts}</EditorSection>;
}
