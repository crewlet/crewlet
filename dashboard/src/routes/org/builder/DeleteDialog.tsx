/**
 * Deleting a seat, or a unit with everything inside it.
 *
 * A DELETE REACHES PAST THE CHART, and the dialog says how far before it
 * happens. Inside the chart the model clears what named the removed nodes (a
 * lead, a `manages` entry, a root seat's unit reference), and the dialog
 * previews the very operation it will dispatch, so the references it lists
 * are the ones the operation clears. Outside the chart:
 *
 * - A removed Datadog fallback seat must be replaced, because the engine
 *   refuses a Datadog block whose `route_to` names no agent seat, and
 *   nowhere else in the builder could fix that refusal. So the dialog asks
 *   for the replacement and writes it with the removal.
 * - A removed seat's GitLab access level is removed with it: the entry is
 *   keyed by handle, and left behind it would grant its level to the next
 *   seat the engine gives that handle.
 * - What exists for the seat at the vendors (a GitHub App, a Slack app, a
 *   Mattermost bot, the GitLab, Datadog or Atlassian account it is enrolled
 *   for) and the secret store entries it references stay until someone
 *   decommissions them, and the dialog lists them by name
 *   (`vendorIdentities`).
 * - The seat's mailbox is retired 24 hours after the engine applies the
 *   removal, its executor is ended then, and its memory is kept and
 *   reattaches to a seat added later under the same handle
 *   (`docs/concepts/seat-ownership.md`, "The removed seat").
 *
 * - A human seat somebody holds, or an open invitation names, is kept by the
 *   engine: the save that removes it is refused (`seat_held`) until the person
 *   moves to another seat or the invitation is cancelled, in Settings › People.
 *   So the dialog says that of every human seat it deletes, and of the
 *   reader's OWN seat it warns that this is them: it is the one delete the
 *   reader can never save as they stand.
 *
 * Nothing is removed until the draft is reviewed and saved, which the dialog
 * says under its first sentence, so a delete reads as the draft change it is.
 *
 * Root seats placed in a removed unit by their `unit:` reference are the
 * operator's to decide (the chart draws them inside the unit, the document
 * holds them at the root), and a removal of more than half the saved
 * company's seats needs its own acknowledgement.
 */

import { useState, type ReactNode } from "react";
import { plural } from "~/lib/format.ts";
import { ConfigField, type FieldChoice } from "~/components/ConfigField.tsx";
import { useAgents, useExecutors } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useBuilder } from "./BuilderContext.tsx";
import {
  acknowledgementChoice,
  EditorSection,
  ReadOnlyNote,
  Refusal,
  ScreenLink,
  StrandedNotes,
  WorkingNotes,
} from "./dialogParts.tsx";
import {
  allSeats,
  handleOf,
  locate,
  managedBy,
  seatWithHandle,
  type DraftSeat,
} from "./model/draft.ts";
import { isRecord } from "./model/json.ts";
import { COMPANY_KEY, isMintedKey, type NodeKey } from "./model/keys.ts";
import { kindOf, type Intent, type ReferenceEffect } from "./model/operations.ts";
import { recordIntent, type BuilderState } from "./model/reducer.ts";
import { datadogFallback } from "./chartModel.ts";
import { isWorking, referenceNames, vendorIdentities } from "./nodeFacts.ts";
import { massRemoval, newlyStranded, removedSeats, removedUnits, simulate } from "./preflight.ts";
import { TrashGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  Callout,
  ChangeGroups,
  ConsequenceList,
  consequencesAnswered,
  ResourceName,
  SegmentedControl,
  type ChangeGroup,
  type ChangeListRow,
  type ConsequenceFact,
  type ConsequenceItem,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { useServedLook } from "../structureLive.tsx";
import { ChoiceFace } from "~/ui/SeatAvatar.tsx";
import { lookOf } from "~/lib/avatar.ts";

type PlacedChoice = "keep" | "remove";

export function DeleteDialog({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const api = useBuilder();
  const served = useServedLook();
  const viewer = useViewer();
  const rows = useAgents();
  const executors = useExecutors();
  const { state } = api;
  const [placedSeats, setPlacedSeats] = useState<PlacedChoice>("keep");
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

  const name = found.node.data.name;
  const company = state.draft.company;

  // What the removal takes, before a replacement fallback is chosen: the
  // replacement cannot be one of the seats it removes.
  const bare = simulate(state, { type: "remove", target: nodeKey, placedSeats });
  const removing = bare.ok ? removedSeats(state.draft, bare.after) : [];
  const fallback = datadogFallback(company);
  const fallbackSeat =
    fallback === undefined ? undefined : removing.find((seat) => handleOf(seat) === fallback);
  const replacements = [...allSeats(state.draft)]
    .map(({ seat }) => seat)
    .filter((seat) => kindOf(seat.data) === "agent" && !removing.includes(seat))
    .map((seat) => ({ seat, handle: handleOf(seat) }));
  // A CHOICE COUNTS ONLY WHILE IT IS STILL ON OFFER. The replacements follow
  // the removal, and "Delete them too" can take the seat that was chosen, so
  // a stale choice would write a fallback naming a seat this very removal
  // deletes: the next check refuses it, and nothing in the builder fixes it.
  const replacement = replacements.some((c) => c.handle === routeTo) ? routeTo : "";

  const intent: Intent = {
    type: "remove",
    target: nodeKey,
    placedSeats,
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
  const people = seats.filter((seat) => kindOf(seat.data) === "human");
  const own = viewer.handle ? people.find((seat) => handleOf(seat) === viewer.handle) : undefined;
  const working = seats
    .filter((seat) => isWorking(handleOf(seat), rows, executors))
    .map((seat) => seat.data.name);

  const acknowledgement: ConsequenceItem[] = mass
    ? [
        acknowledgementChoice("mass_removal", {
          checked: acknowledged,
          onCheckedChange: setAcknowledged,
          massRemoval: mass,
        }),
      ]
    : [];

  const blocked =
    !preview.ok ||
    api.readOnly ||
    (fallbackSeat !== undefined && replacement === "") ||
    !consequencesAnswered(acknowledgement);

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
      icon={<TrashGlyph />}
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
          <Button variant="ghost" onClick={onClose}>
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
        Nothing changes until you review and save.
      </p>
      <HeldSeats seats={people} own={own} />

      {bare.ok && bare.op.type === "remove" && bare.op.placed.length > 0 && (
        <EditorSection
          title="Seats placed here by unit reference"
          hint={`These seats sit at the top level but point to this team: ${bare.op.placed
            .map((p) => (isRecord(p.json) && typeof p.json.name === "string" ? p.json.name : p.key))
            .join(", ")}.`}
        >
          <SegmentedControl<PlacedChoice>
            label="Seats placed here by unit reference"
            semantics="radio"
            value={placedSeats}
            onValueChange={setPlacedSeats}
            options={[
              { value: "keep", label: "Keep them at the top level" },
              { value: "remove", label: "Delete them too" },
            ]}
          />
        </EditorSection>
      )}

      <ClearedReferences
        state={state}
        cleared={cleared.filter((c) => c.kind !== "gitlab_access_level")}
      />

      <OutsideTheChart
        state={state}
        agents={agents}
        fallbackSeat={fallbackSeat}
        replacements={replacements.map((c) => ({
          value: c.handle,
          label: c.seat.data.name,
          leading: (
            <ChoiceFace
              seat={{
                name: c.seat.data.name,
                kind: "agent",
                avatar: lookOf(c.seat.data.avatar, served(c.handle)),
              }}
            />
          ),
        }))}
        routeTo={replacement}
        onRouteTo={setRouteTo}
        accessLevels={op?.accessLevels ?? []}
      />

      <StrandedNotes stranded={stranded} />
      <WorkingNotes names={working} />

      {acknowledgement.length > 0 && <ConsequenceList items={acknowledgement} />}
    </Modal>
  );
}

/**
 * What a delete of human seats meets on save: the engine keeps a seat
 * somebody holds or an invitation names, and refuses the save until the
 * person moves or the invitation is cancelled. The reader's own seat is said
 * as theirs, in the warning tone: they hold it, so that save is refused for
 * certain, and the access their seat gives them is what they would be giving
 * up.
 */
function HeldSeats({ seats, own }: { seats: readonly DraftSeat[]; own: DraftSeat | undefined }) {
  if (seats.length === 0) return null;
  const people = <ScreenLink to="people">Open People</ScreenLink>;
  if (own) {
    return (
      <Callout variant="warning" title="This is your own seat">
        You hold {own.data.name}, and your access comes with it. A seat somebody holds is kept, so
        saving this is refused until you move to another seat in People. {people}
      </Callout>
    );
  }
  return (
    <p className="t-body">
      {seats.length === 1
        ? "If somebody holds this seat or an invitation names it, saving is refused until they move to another seat or the invitation is cancelled in People."
        : "If somebody holds one of these seats or an invitation names one, saving is refused until they move to another seat or the invitation is cancelled in People."}{" "}
      {people}
    </p>
  );
}

/** The names of the nodes a reference effect's holder and target refer to. */
function holderName(state: BuilderState, key: NodeKey): string {
  if (key === COMPANY_KEY) return "The company";
  const found = locate(state.draft, key);
  return found ? found.node.data.name : key;
}

function ClearedReferences({
  state,
  cleared,
}: {
  state: BuilderState;
  cleared: readonly ReferenceEffect[];
}) {
  if (cleared.length === 0) return null;
  // A REFERENCE NAMES AN IDENTITY, a handle or a unit key, which is said as
  // the name of what it named in the draft before the removal.
  const named = (from: string): string => managedBy(state.draft, from)?.node.data.name || from;
  const groups = new Map<string, ChangeGroup & { rows: ChangeListRow[] }>();
  for (const effect of cleared) {
    const row: ChangeListRow =
      effect.kind === "lead"
        ? {
            kind: "removed",
            label: "Lead",
            from: seatWithHandle(state.draft, effect.from)?.data.name || effect.from,
          }
        : effect.kind === "manages"
          ? { kind: "removed", label: "Manages", from: named(effect.from) }
          : effect.kind === "unit"
            ? {
                kind: "removed",
                label: "Unit reference",
                from: named(effect.from),
                note: "Stays at the top level.",
              }
            : { kind: "removed", label: "Reference", from: effect.from };
    const group = groups.get(effect.holder) ?? {
      key: effect.holder,
      subject: holderName(state, effect.holder),
      rows: [],
    };
    group.rows.push(row);
    groups.set(effect.holder, group);
  }
  return <ChangeGroups heading="References that are cleared" groups={[...groups.values()]} />;
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
  replacements: FieldChoice[];
  routeTo: string;
  onRouteTo: (handle: string) => void;
  accessLevels: readonly { handle: string; before?: string }[];
}) {
  // A seat this draft created was never saved: nothing exists for it at a
  // vendor, no entry was written for it, and it has no mailbox.
  const saved = agents.filter((seat) => !isMintedKey(seat.key));
  const identities = saved
    .map((seat) => ({
      key: seat.key,
      name: seat.data.name,
      made: vendorIdentities(state, seat),
      references: referenceNames(seat.data),
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
        <Callout
          key="datadog"
          variant="danger"
          action={
            <ScreenLink to="integrations" standalone>
              Open Integrations
            </ScreenLink>
          }
        >
          {fallbackSeat.data.name} is the Datadog fallback and no other agent can take over. Add an
          agent first, or disconnect Datadog.
        </Callout>
      ) : (
        <ConfigField
          key="datadog"
          label="Datadog fallback"
          kind="choice"
          choices={replacements}
          value={routeTo}
          onChange={onRouteTo}
          help="Choose the agent that takes untagged Datadog alerts."
        />
      ),
    );
  }
  const facts: ConsequenceFact[] = accessLevels.map((level) => ({
    kind: "fact",
    key: `gitlab-${level.handle}`,
    tone: "danger",
    text: `${level.handle}’s GitLab access level${level.before ? ` (${level.before})` : ""} is removed.`,
  }));
  if (identities.length > 0) {
    facts.push(
      {
        kind: "fact",
        key: "decommission",
        tone: "info",
        text: (
          <>
            These stay until you decommission them.{" "}
            <ScreenLink to="integrations">Open Integrations</ScreenLink>{" "}
            <ScreenLink to="secrets">Open Secrets</ScreenLink>
          </>
        ),
      },
      ...identities.map(({ key, name, made, references }): ConsequenceFact => ({
        kind: "fact",
        key: `vendors-${key}`,
        tone: "kept",
        text: (
          <>
            {name}
            {made.length > 0 && `: ${made.join(", ")}`}
            {references.length > 0 && (
              <>
                {made.length > 0 ? "; " : ": "}secret store entries{" "}
                {references.map((ref, i) => (
                  <span key={ref}>
                    {i > 0 && ", "}
                    <ResourceName>{ref}</ResourceName>
                  </span>
                ))}
              </>
            )}
          </>
        ),
      })),
    );
  }
  if (saved.length > 0) {
    facts.push({
      kind: "fact",
      key: "mailbox",
      tone: "kept",
      text:
        saved.length === 1
          ? "Its mail is kept for 24 hours. Its memory is kept for a seat with the same handle."
          : "Their mail is kept for 24 hours. Their memory is kept for seats with the same handles.",
    });
  }
  if (facts.length > 0) parts.push(<ConsequenceList key="facts" items={facts} />);
  if (parts.length === 0) return null;
  return <EditorSection title="Outside the chart">{parts}</EditorSection>;
}
