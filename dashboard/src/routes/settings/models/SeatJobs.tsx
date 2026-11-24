/**
 * Under the models: a fleet with no room said once (its alarm), the Fleets
 * card, then who runs on which model, and the confirms their actions open.
 * Every fleet change is one `llm-fleets` write, read back with the page.
 */

import { useState } from "react";
import { useToast } from "@crewlethq/ui";
import { useNavigator } from "~/app/router.tsx";
import type { Fleet, FleetMember } from "~/protocol/fleets.ts";
import type { ModelsData } from "./data.ts";
import { notSaved } from "./flows/SeatMove.tsx";
import { entityOf, saveFleet } from "./fleets/data.ts";
import { FleetsTree } from "./fleets/FleetsTree.tsx";
import { DeleteFleetConfirm, FullFleets, RemoveMemberConfirm } from "./fleets/parts.tsx";
import { WhoRunsTable } from "./fleets/WhoRuns.tsx";

export function WhoRuns({ data }: { data: ModelsData }) {
  const nav = useNavigator();
  const toast = useToast();
  const [deleting, setDeleting] = useState<Fleet | null>(null);
  const [removing, setRemoving] = useState<{ fleet: Fleet; member: FleetMember } | null>(null);
  const fleets = data.fleets;
  if (!fleets) return null;

  /** A fleet's members written in a new order or set; the refusal in words, or "" once saved. */
  const writeMembers = async (f: Fleet, members: readonly string[], summary: string) => {
    const outcome = await saveFleet(f.key, entityOf(f, members), summary);
    if (outcome.kind !== "saved" && outcome.kind !== "unchanged")
      return notSaved(outcome, `${f.name} may or may not have changed`);
    data.reload();
    return "";
  };
  // A NEW ENTRY, as the router starts every new screen: at the top.
  const settings = (f: Fleet) => nav.to(["settings", "models"], { fleet: f.key, step: "members" });

  return (
    <>
      <FullFleets fleets={fleets} />
      <FleetsTree
        fleets={fleets}
        data={data}
        onSettings={settings}
        onCreate={() => nav.to(["settings", "models"], { fleet: "new" })}
        onDelete={setDeleting}
        onRemove={(fleet, member) => setRemoving({ fleet, member })}
        onReorder={(f, members) => {
          void writeMembers(f, members, `Reorder the ${f.name} fleet`).then((why) => {
            if (why) toast.show({ variant: "danger", message: why });
          });
        }}
      />
      <WhoRunsTable data={data} fleets={fleets} />
      {deleting ? (
        <DeleteFleetConfirm
          fleet={deleting}
          fleets={fleets}
          data={data}
          onClose={() => setDeleting(null)}
        />
      ) : null}
      {removing ? (
        <RemoveMemberConfirm
          fleet={removing.fleet}
          member={removing.member}
          fleets={fleets}
          onClose={() => setRemoving(null)}
          onRemove={() => {
            const { fleet, member } = removing;
            const rest = fleet.members.map((m) => m.entry).filter((e) => e !== member.entry);
            return writeMembers(fleet, rest, `Remove ${member.entry} from the ${fleet.name} fleet`);
          }}
          onUndo={() => {
            const { fleet, member } = removing;
            void writeMembers(
              fleet,
              fleet.members.map((m) => m.entry),
              `Put ${member.entry} back in the ${fleet.name} fleet`,
            ).then((why) => {
              if (why) toast.show({ variant: "danger", message: why });
            });
          }}
        />
      ) : null}
    </>
  );
}
