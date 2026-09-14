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
 * - What the engine made for the seat at the vendors (a GitHub App, a Slack
 *   app, a Mattermost bot, a GitLab or Atlassian account) and the secret
 *   store entries it references stay until someone decommissions them, and
 *   the dialog lists them by name.
 * - The seat's mailbox is retired 24 hours after the engine applies the
 *   removal, its coding runs are ended then, and its memory is kept and
 *   reattaches to a seat added later under the same handle
 *   (`docs/concepts/seat-ownership.md`, "The removed seat").
 *
 * Root seats placed in a removed unit by their `unit:` reference are the
 * operator's to decide (the chart draws them inside the unit, the document
 * holds them at the root), and a removal of more than half the saved
 * company's seats needs its own acknowledgement.
 */

import { useState, type ReactNode } from "react";
import { plural } from "~/lib/format.ts";
import { Checkbox } from "~/ui/Checkbox.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { Field } from "~/ui/Field.tsx";
import { Button, Segmented } from "~/ui/primitives.tsx";
import { useBuilder } from "./BuilderContext.tsx";
import {
  EditorSection,
  NameList,
  Refusal,
  ScreenLink,
  StrandedNotes,
  WorkingNotes,
} from "./dialogParts.tsx";
import { allSeats, locate, type DraftSeat } from "./model/draft.ts";
import { getPath, isRecord } from "./model/json.ts";
import { COMPANY_KEY, isMintedKey, type NodeKey } from "./model/keys.ts";
import { kindOf, type Intent, type ReferenceEffect } from "./model/operations.ts";
import { recordIntent, type BuilderState } from "./model/reducer.ts";
import {
  datadogFallback,
  handleOf,
  hasGitLabProvisioning,
  isConnected,
  isWorking,
  referenceNames,
} from "./nodeFacts.ts";
import { massRemoval, newlyStranded, removedSeats, removedUnits, simulate } from "./preflight.ts";

type PlacedChoice = "keep" | "remove";

export function DeleteDialog({ nodeKey, onClose }: { nodeKey: NodeKey; onClose: () => void }) {
  const api = useBuilder();
  const { state } = api;
  const [placedSeats, setPlacedSeats] = useState<PlacedChoice>("keep");
  const [routeTo, setRouteTo] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const found = locate(state.draft, nodeKey);

  if (!found) {
    return (
      <Dialog title="Delete" onClose={onClose} footer={<Button onClick={onClose}>Close</Button>}>
        <p className="t-body">This node is no longer in the draft.</p>
      </Dialog>
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
    fallback === undefined
      ? undefined
      : removing.find((seat) => handleOf(state, seat.key) === fallback);
  const replacements = [...allSeats(state.draft)]
    .map(({ seat }) => seat)
    .filter((seat) => kindOf(seat.data) === "agent" && !removing.includes(seat))
    .map((seat) => ({ seat, handle: handleOf(state, seat.key) }))
    .filter((c): c is { seat: DraftSeat; handle: string } => c.handle !== undefined);

  const intent: Intent = {
    type: "remove",
    target: nodeKey,
    placedSeats,
    ...(fallbackSeat && routeTo !== "" ? { routeTo } : {}),
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
    .filter((seat) => isWorking(handleOf(state, seat.key), api.agents, api.sandboxes))
    .map((seat) => seat.data.name);

  const blocked =
    !preview.ok ||
    api.readOnly ||
    (fallbackSeat !== undefined && routeTo === "") ||
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
    <Dialog
      title={`Delete ${name}`}
      icon="trash"
      width={560}
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
        Undo brings it back until the draft is saved.
      </p>

      {bare.ok && bare.op.type === "remove" && bare.op.placed.length > 0 && (
        <EditorSection
          title="Seats placed here by unit reference"
          hint="These seats are declared at the top level with a unit reference to this unit, so the chart draws them inside it."
        >
          <NameList
            names={bare.op.placed.map((p) =>
              isRecord(p.json) && typeof p.json.name === "string" ? p.json.name : p.key,
            )}
          />
          <Segmented<PlacedChoice>
            ariaLabel="Seats placed here by unit reference"
            semantics="radio"
            value={placedSeats}
            onChange={setPlacedSeats}
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
        replacements={replacements.map((c) => ({ value: c.handle, label: c.seat.data.name }))}
        routeTo={routeTo}
        onRouteTo={setRouteTo}
        accessLevels={op?.accessLevels ?? []}
      />

      <StrandedNotes stranded={stranded} />
      <WorkingNotes names={working} />

      {mass && (
        <Checkbox
          framed
          tone="critical"
          label={`Delete ${mass.removed} of the ${plural(mass.total, "seat")} the company has`}
          description="More than half of the saved company's seats would go. Confirm this is the change you mean."
          checked={acknowledged}
          onChange={setAcknowledged}
        />
      )}
    </Dialog>
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
  const sentence = (effect: ReferenceEffect): string => {
    const holder = holderName(state, effect.holder);
    switch (effect.kind) {
      case "lead":
        return `${holder} no longer has ${effect.from} as its lead.`;
      case "manages":
        return `${holder} no longer manages ${effect.from}.`;
      case "unit":
        return `${holder} loses its unit reference to ${effect.from} and stays at the top level.`;
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

/** What the engine made for a removed agent seat at the vendors, by name. */
function vendorIdentities(state: BuilderState, seat: DraftSeat): string[] {
  const company = state.draft.company;
  const out: string[] = [];
  const github = getPath(seat.data, ["integrations", "github"]);
  if (isRecord(github) && typeof github.app_slug === "string" && github.app_slug !== "") {
    out.push(`the GitHub App ${github.app_slug}`);
  }
  if (isRecord(getPath(seat.data, ["integrations", "slack"]))) out.push("its Slack app");
  if (isRecord(getPath(seat.data, ["integrations", "mattermost"]))) out.push("its Mattermost bot");
  if (hasGitLabProvisioning(company)) out.push("its GitLab service account");
  if (isConnected(company, "atlassian")) out.push("its Atlassian account");
  return out;
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
  const saved = agents.filter((seat) => !isMintedKey(seat.key));
  const identities = agents
    .map((seat) => ({
      seat,
      made: vendorIdentities(state, seat),
      references: referenceNames(seat.data),
    }))
    .filter((entry) => entry.made.length > 0 || entry.references.length > 0);
  const parts: ReactNode[] = [];

  if (fallbackSeat) {
    parts.push(
      <Field
        key="datadog"
        label="Datadog fallback"
        kind="choice"
        choices={replacements}
        value={routeTo}
        onChange={onRouteTo}
        help={`${fallbackSeat.data.name} is the Datadog fallback: an alert whose tags name no seat wakes it. Choose the agent seat that takes over; the engine refuses a Datadog fallback that names no agent seat.`}
      />,
    );
  }
  if (accessLevels.length > 0) {
    parts.push(
      <NameList
        key="gitlab"
        names={accessLevels.map(
          (level) =>
            `The GitLab access level for ${level.handle}${level.before ? ` (${level.before})` : ""} is removed, so it cannot pass to a seat added later under that handle.`,
        )}
      />,
    );
  }
  if (identities.length > 0) {
    parts.push(
      <div key="vendors" className="col gap-2">
        <p className="t-body">
          These stay until you decommission them.{" "}
          <ScreenLink to={["integrations"]}>Open Integrations</ScreenLink>{" "}
          <ScreenLink to={["secrets"]}>Open Secrets</ScreenLink>
        </p>
        <ul className="builder-list">
          {identities.map(({ seat, made, references }) => (
            <li key={seat.key}>
              {seat.data.name}
              {made.length > 0 && `: ${made.join(", ")}`}
              {references.length > 0 && (
                <>
                  {made.length > 0 ? "; " : ": "}secret store entries{" "}
                  {references.map((ref, i) => (
                    <span key={ref}>
                      {i > 0 && ", "}
                      <code className="inline">{ref}</code>
                    </span>
                  ))}
                </>
              )}
            </li>
          ))}
        </ul>
      </div>,
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
        {one ? "Its memory is" : "Their memory is"} kept, and a seat added later with the same
        handle reattaches to it.
      </p>,
    );
  }
  if (parts.length === 0) return null;
  return <EditorSection title="Outside the chart">{parts}</EditorSection>;
}
