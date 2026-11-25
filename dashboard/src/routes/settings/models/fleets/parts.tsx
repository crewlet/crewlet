/**
 * The Fleets card's working parts: who a fleet or member serves now, a
 * member's status and limits (the Models table's own cells), a fleet's limits
 * averaged over its plans, the alarm for a fleet with no room, and the remove
 * and delete confirms.
 */

import { useState } from "react";
import {
  AvatarStack,
  Callout,
  FormField,
  LimitMeters,
  Select,
  Text,
  Tooltip,
  formatRelative,
  usageTone,
  useToast,
  type AvatarStackMember,
  type LimitWindow,
  type SelectOption,
} from "@crewlethq/ui";
import assistantMark from "@crewlethq/icons/svg/product/small/assistant.svg?url";
import advisorMark from "@crewlethq/icons/svg/product/small/advisor.svg?url";
import { useNow } from "~/lib/clock.ts";
import { DEFAULT_FLEET, fleetLabel, fleetRef } from "~/lib/fleets.ts";
import { listed } from "~/lib/format.ts";
import { seatBadge } from "~/ui/SeatAvatar.tsx";
import { ConfirmModal } from "~/components/Modal.tsx";
import type { Fleet, FleetAssignment, FleetMember, FleetsAnswer } from "~/protocol/fleets.ts";
import type { ModelsData } from "../data.ts";
import { HealthOneLine } from "../parts.tsx";
import { UsageLimitsCell } from "../UsageLimits.tsx";
import { NO_LIMITS } from "../words.ts";
import { notSaved } from "../flows/SeatMove.tsx";
import { classOf, deleteFleet, namesOf, onFleet, verbFor } from "./data.ts";

/** The helpers by handle, their names and product marks, as images the kit stack takes. */
const HELPERS: Record<string, { name: string; mark: string }> = {
  assistant: { name: "Ask Crewlet", mark: assistantMark },
  advisor: { name: "Advisor", mark: advisorMark },
};

/** Work in flight the nodes' records counted but could not name, in words. */
const unnamed = (n: number) => `${n} more ${n === 1 ? "call" : "calls"} not named`;

/**
 * Who a fleet or member serves now: four faces and "+N", the names on hover;
 * "Nobody" when none. Work the nodes' records could not name is counted after
 * them, never drawn as nobody.
 */
export function ServingFaces({
  handles,
  unlisted = 0,
  data,
}: {
  handles: readonly string[];
  unlisted?: number;
  data: ModelsData;
}) {
  if (handles.length === 0)
    return (
      <Text as="span" variant="caption" tone="tertiary">
        {unlisted > 0 ? unnamed(unlisted) : "Nobody"}
      </Text>
    );
  const members: AvatarStackMember[] = handles.map((h) => {
    const helper = HELPERS[h];
    if (helper) return { id: h, name: helper.name, kind: "agent", src: helper.mark };
    const s = data.seats.find((x) => x.handle === h);
    return s ? { id: h, ...seatBadge(s.name || h, s.kind, s.avatar) } : { id: h, name: h };
  });
  const names = listed(members.map((m) => m.name));
  const said = unlisted > 0 ? `${names}, and ${unnamed(unlisted)}` : names;
  return (
    <Tooltip content={said} placement="top">
      <span tabIndex={0} aria-label={`Serving ${said}`}>
        <AvatarStack members={members} size="xs" max={4} decorative />
      </span>
    </Tooltip>
  );
}

/** A member's status, as the Models table says its model's. */
export function MemberStatus({ entry, data }: { entry: string; data: ModelsData }) {
  const model = data.models.find((x) => x.key === entry);
  return model ? <HealthOneLine model={model} /> : null;
}

/** A member's usage limits, the Models table's own cell. */
export function MemberLimits({ entry, data }: { entry: string; data: ModelsData }) {
  const model = data.models.find((x) => x.key === entry);
  if (!model)
    return <LimitMeters label={`Usage limits of ${entry}`} windows={[]} empty={NO_LIMITS} />;
  return <UsageLimitsCell model={model} />;
}

/** The highest share a model's sign-in reports for one window, or null where it reports none. */
function usedIn(data: ModelsData, entry: string, window: "session" | "weekly"): number | null {
  const used = (data.models.find((m) => m.key === entry)?.entry.limits ?? [])
    .filter((l) => l.window === window && l.used !== null)
    .map((l) => l.used!);
  return used.length ? Math.max(...used) : null;
}

/** A fleet's usage: each window's average over its members on a plan. */
export function FleetLimits({ fleet, data }: { fleet: Fleet; data: ModelsData }) {
  const plans = fleet.members.filter((m) => m.kind === "plan");
  const windows: LimitWindow[] = [];
  for (const [id, label] of [
    ["session", "5-hour"],
    ["weekly", "Weekly"],
  ] as const) {
    const each = plans.map((m) => usedIn(data, m.entry, id)).filter((u): u is number => u !== null);
    if (each.length === 0) continue;
    const used = each.reduce((sum, u) => sum + Math.min(u, 1), 0) / each.length;
    const tone = usageTone(used);
    windows.push({
      id,
      label,
      used,
      state: tone === "danger" ? "refusing" : tone === "warning" ? "near" : "ok",
      name: `${label} limit, ${Math.round(used * 100)}% used on average over ${each.length} plans`,
    });
  }
  return (
    <LimitMeters
      label={`Usage limits of ${fleet.name}`}
      windows={windows}
      empty={NO_LIMITS}
      okTone="success"
    />
  );
}

/** Whether no member of a fleet takes new work, while it has members. */
const full = (f: Fleet) => f.members.length > 0 && f.members.every((m) => m.state !== "available");

/** Who waits on a fleet: a seat or helper with a job on it that runs on nothing now. */
function waitingOn(assignments: readonly FleetAssignment[], key: string): FleetAssignment[] {
  return assignments.filter((a) =>
    (a.jobs ?? [a]).some(
      (j) =>
        "fleet" in j.target &&
        j.target.fleet === key &&
        j.now === null &&
        j.now_kind !== "unlisted",
    ),
  );
}

/** One alarm per fleet with no room: who waits, the first room, and the ways out. */
export function FullFleets({ fleets }: { fleets: FleetsAnswer }) {
  const now = useNow();
  return (
    <>
      {fleets.fleets.filter(full).map((f) => {
        const waiting = waitingOn(fleets.assignments, f.key);
        if (waiting.length === 0) return null;
        const first = f.members
          .filter((m): m is FleetMember & { until: string } => m.until !== null)
          .sort((a, b) => a.until.localeCompare(b.until))[0];
        const when = first ? formatRelative(first.until, now) : null;
        return (
          <Callout key={f.key} variant="danger" title={`Every member of ${f.name} is at its limit`}>
            {`${verbFor(
              waiting.map((a) => a.name),
              "waits",
              "wait",
            )}. Their work so far is kept${
              first && when ? `; the first room is ${when} (${first.entry})` : ""
            }. Add a member, or move them to a weaker model on purpose.`}
          </Callout>
        );
      })}
    </>
  );
}

/**
 * Removing a member asks once, saying what follows: who it serves on this
 * fleet moves to the other members, or waits where it was the last; who is
 * pinned to it stays. Undo puts it back where it was.
 */
export function RemoveMemberConfirm({
  fleet,
  member,
  fleets,
  onClose,
  onRemove,
  onUndo,
}: {
  fleet: Fleet;
  member: FleetMember;
  fleets: FleetsAnswer;
  onClose: () => void;
  onRemove: () => Promise<string>;
  onUndo: () => void;
}) {
  const toast = useToast();
  const [refused, setRefused] = useState("");
  const on = new Set(onFleet(fleets.assignments, fleet.key).map((a) => a.subject));
  // ON THIS MEMBER: its work runs here now, or its next turn goes here.
  const here = new Set(
    fleets.assignments
      .filter((a) => [a, ...(a.jobs ?? [])].some((j) => j.now === member.entry))
      .map((a) => a.subject),
  );
  const moving = namesOf(
    [...on].filter((h) => here.has(h)),
    fleets.assignments,
  );
  // PINNED TO THIS MODEL, not served through another fleet it is also in.
  const pinnedHere = new Set(
    fleets.assignments
      .filter((a) =>
        (a.jobs ?? [a]).some((j) => "pinned" in j.target && j.target.pinned === member.entry),
      )
      .map((a) => a.subject),
  );
  const pinned = namesOf(
    [...pinnedHere].filter((h) => !on.has(h)),
    fleets.assignments,
  );
  const last = fleet.members.length === 1;
  // THE LAST MEMBER: everyone on the fleet waits until one joins.
  const waiting = last ? namesOf([...on], fleets.assignments) : [];
  const serving = last
    ? waiting.length
      ? `${verbFor(waiting, "waits", "wait")} until a member joins; a running turn finishes first.`
      : "Nobody on this fleet runs on it now, so no seat moves."
    : moving.length
      ? `${verbFor(moving, "moves", "move")} to the other members at ${
          moving.length === 1 ? "its" : "their"
        } next turn; a running turn finishes first.`
      : member.unlisted_serving > 0
        ? "Work its status records could not name runs on it; it moves to the other members at its next turn, and a running turn finishes first."
        : "Nobody on this fleet runs on it now, so no seat moves.";
  return (
    <ConfirmModal
      open
      title={`Remove ${member.entry} from ${fleet.name}?`}
      message={`${member.entry} stops taking work from this fleet.`}
      consequences={[
        { kind: "fact", key: "serving", tone: "follows", text: serving },
        ...(pinned.length
          ? [
              {
                kind: "fact" as const,
                key: "pinned",
                tone: "kept" as const,
                text: `${verbFor(pinned, "is", "are")} pinned to ${member.entry} and ${
                  pinned.length === 1 ? "stays" : "stay"
                } on it.`,
              },
            ]
          : []),
        {
          kind: "fact",
          key: "kept",
          tone: "kept",
          text: `${member.entry} stays under Models with its sign-in, and nothing else changes.`,
        },
      ]}
      confirmLabel="Remove from fleet"
      destructive
      onConfirm={async () => {
        const why = await onRemove();
        if (why) {
          setRefused(why);
          return;
        }
        onClose();
        toast.show({
          variant: "success",
          message: `${member.entry} removed from ${fleet.name}.`,
          action: { label: "Undo", onClick: onUndo },
        });
      }}
      onClose={onClose}
    >
      {refused ? (
        <Callout variant="danger" title="Not removed" live="polite">
          {refused}
        </Callout>
      ) : null}
    </ConfirmModal>
  );
}

/**
 * Deleting a fleet asks once: who runs on it and where they move (another
 * fleet or a model of its class, or the default fleet), and that its members
 * stay under Models.
 */
export function DeleteFleetConfirm({
  fleet,
  fleets,
  data,
  onClose,
}: {
  fleet: Fleet;
  fleets: FleetsAnswer;
  data: ModelsData;
  onClose: () => void;
}) {
  const toast = useToast();
  const [refused, setRefused] = useState("");
  const names = onFleet(fleets.assignments, fleet.key).map((a) => a.name);
  const sameClass = (model: string) => !fleet.class || classOf(model) === fleet.class;
  const others = fleets.fleets.filter((f) => f.key !== fleet.key);
  const options: SelectOption[] = [
    ...others
      .filter((f) => !f.is_default && f.class === fleet.class)
      .map((f) => ({ value: fleetRef(f.key), label: fleetLabel(f) })),
    ...data.models
      .filter((m) => m.harness === (fleet.harness || m.harness) && sameClass(m.model))
      .map((m) => ({ value: m.key, label: m.key })),
    ...others
      .filter((f) => f.is_default)
      .map((f) => ({ value: fleetRef(f.key), label: fleetLabel(f) })),
  ];
  const [moveTo, setMoveTo] = useState(String(options[0]?.value ?? fleetRef(DEFAULT_FLEET)));
  const members = fleet.members.map((m) => m.entry);

  async function remove() {
    setRefused("");
    const outcome = await deleteFleet(fleet.key, moveTo, `Delete the ${fleet.name} fleet`);
    if (outcome.kind !== "saved") {
      setRefused(notSaved(outcome, "The fleet may or may not have been deleted"));
      return;
    }
    data.reload();
    onClose();
    toast.show({ variant: "success", message: `${fleet.name} fleet deleted.` });
  }

  return (
    <ConfirmModal
      open
      title={`Delete ${fleet.name}?`}
      message={
        names.length
          ? `${verbFor(names, "runs", "run")} on this fleet.`
          : "Nothing runs on this fleet."
      }
      moveTo={
        names.length ? (
          <FormField label={names.length === 1 ? "Move it to" : "Move them to"}>
            {(field) => (
              <Select
                id={field.id}
                value={moveTo}
                options={options}
                onChange={(next) => setMoveTo(String(next))}
              />
            )}
          </FormField>
        ) : undefined
      }
      consequences={
        members.length
          ? [
              {
                kind: "fact",
                key: "kept",
                tone: "kept",
                text: `Its members (${listed(members)}) stay under Models with their sign-ins, and nothing else changes.`,
              },
            ]
          : []
      }
      confirmLabel="Delete fleet"
      destructive
      onConfirm={remove}
      onClose={onClose}
    >
      {refused ? (
        <Callout variant="danger" title="Not deleted" live="polite">
          {refused}
        </Callout>
      ) : null}
    </ConfirmModal>
  );
}
