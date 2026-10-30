/**
 * The Agents tab's dialogs.
 *
 * - THE REVIEW, before a save: per agent, what the draft changes (from and to,
 *   in the vendor's words), what turning an agent off does there, and what the
 *   next sync will do at the vendor because of it, step by step and marked as
 *   Crewlet's or a person's. The plan is the engine's (`POST .../access/plan`),
 *   so the preview and the sync cannot disagree. Where the plan says a save
 *   grants administration access, the review says the save asks for a fresh
 *   confirmation of who you are (D10); the transport asks for it when the
 *   engine refuses.
 * - THE REMOVAL of one agent's account, allowed only once it is off (D1).
 * - GITHUB'S PERMISSIONS for one agent, edited into the draft in GitHub's own
 *   words, so the row stays at one size.
 */

import { useEffect, useState } from "react";
import {
  Button,
  Callout,
  ChangeList,
  DescriptionList,
  Modal,
  SegmentedControl,
  Spinner,
  Tag,
  type ChangeListRow,
} from "@crewlethq/ui";
import type {
  AccessAnswer,
  AccessDesired,
  AccessDraft,
  AccessPlanAnswer,
  AccessSeat,
  AccessStep,
  GitHubAccess,
} from "~/contract/access.ts";
import { accessApi, refusalWords } from "./api.ts";
import { lanesOf, levelName, levelsHeading, scopeWords, type Lane } from "./logic.ts";
import { GITHUB_ACCESS_NAMES, laneWords } from "./words.ts";

function rows(lane: Lane, from: AccessDesired, to: AccessDesired): ChangeListRow[] {
  const out: ChangeListRow[] = [];
  const access = laneWords(lane.key).access;
  if (from.enabled !== to.enabled) {
    out.push({
      kind: "changed",
      label: `On ${lane.name}`,
      from: from.enabled ? "On" : "Off",
      to: to.enabled ? "On" : "Off",
    });
    if (!to.enabled) return out;
    // TURNED ON: what it is turned on with.
    if (lane.levels.length > 0)
      out.push({ kind: "fact", label: access, value: levelName(lane, to.level) });
    for (const perm of lane.permissions ?? []) {
      const held = to.permissions?.[perm.key];
      if (held) out.push({ kind: "fact", label: perm.name, value: GITHUB_ACCESS_NAMES[held] });
    }
    if (lane.scope !== undefined)
      out.push({
        kind: "fact",
        label: laneWords(lane.key).scope?.noun ?? "Scope",
        value: scopeWords(lane, to.scope),
      });
    return out;
  }
  if (!to.enabled) return out;
  if (lane.levels.length > 0 && from.level !== to.level) {
    out.push({
      kind: "changed",
      label: access,
      from: levelName(lane, from.level),
      to: levelName(lane, to.level),
    });
  }
  for (const perm of lane.permissions ?? []) {
    const a = from.permissions?.[perm.key] ?? "none";
    const b = to.permissions?.[perm.key] ?? "none";
    if (a !== b)
      out.push({
        kind: "changed",
        label: perm.name,
        from: GITHUB_ACCESS_NAMES[a],
        to: GITHUB_ACCESS_NAMES[b],
      });
  }
  if (lane.scope !== undefined && scopeWords(lane, from.scope) !== scopeWords(lane, to.scope)) {
    out.push({
      kind: "changed",
      label: laneWords(lane.key).scope?.noun ?? "Scope",
      from: scopeWords(lane, from.scope),
      to: scopeWords(lane, to.scope),
    });
  }
  return out;
}

/** The steps a sync takes, each marked as Crewlet's or a person's. */
export function Steps({ steps }: { steps: AccessStep[] }) {
  return (
    <ol className="agent-access-plan">
      {steps.map((s, i) => (
        <li key={i} className="agent-access-plan-step">
          <Tag size="xs" variant={s.by === "you" ? "warning" : "neutral"} appearance="outline">
            {s.by === "you" ? "A person" : "Crewlet"}
          </Tag>
          <span>
            {s.text}.{s.note && <span className="agent-access-plan-note"> {s.note}</span>}
          </span>
        </li>
      ))}
    </ol>
  );
}

export function ReviewDialog({
  kind,
  name,
  data,
  draft,
  onClose,
  onSaved,
}: {
  kind: string;
  name: string;
  data: AccessAnswer;
  draft: AccessDraft;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [plan, setPlan] = useState<AccessPlanAnswer | null>(null);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let live = true;
    accessApi.plan(kind, draft).then(
      (p) => {
        if (live) setPlan(p);
      },
      (err: unknown) => {
        if (live) setError(refusalWords(err));
      },
    );
    return () => {
      live = false;
    };
  }, [kind, draft]);

  const lanes = lanesOf(data);
  const changed = data.seats.flatMap((seat) =>
    lanes
      .filter((lane) => draft[seat.handle]?.[lane.key] && seat.lanes[lane.key])
      .map((lane) => ({
        seat,
        lane,
        from: seat.lanes[lane.key]!.desired,
        to: draft[seat.handle]![lane.key]!,
      })),
  );
  const people = plan?.plans.some((p) => p.steps.some((s) => s.by === "you")) ?? false;
  const raises = changed.filter(
    ({ seat, lane }) =>
      plan?.plans.find((p) => p.handle === seat.handle && p.lane === lane.key)?.step_up,
  );

  return (
    <Modal
      open
      size="lg"
      onClose={onClose}
      title={`Review changes to ${name}`}
      subtitle={`Nothing changes at ${name} until you save. Saving writes one revision of the company document and starts a sync.`}
      footerStart={
        people ? (
          <span className="t-caption">Steps marked A person need somebody at {name}.</span>
        ) : undefined
      }
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Back
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={saving}
            disabledReason={plan ? undefined : "Reading what the sync will do."}
            onClick={async () => {
              setSaving(true);
              setError("");
              try {
                await accessApi.save(kind, draft, data.revision);
                onSaved();
              } catch (err) {
                setError(refusalWords(err));
                setSaving(false);
              }
            }}
          >
            Save and sync
          </Button>
        </>
      }
    >
      <div className="agent-access-review">
        {error && (
          <Callout variant="danger" live="polite">
            {error}
          </Callout>
        )}
        {plan?.step_up && (
          <Callout variant="warning" title="Administration access">
            {raises.length > 0
              ? `${raises.map(({ seat }) => seat.name).join(", ")} ${raises.length === 1 ? "gets" : "get"} administration access at ${name}. Saving asks you to confirm it is you.`
              : `This change grants administration access at ${name}. Saving asks you to confirm it is you.`}
          </Callout>
        )}
        {changed.map(({ seat, lane, from, to }) => {
          const steps = plan?.plans.find(
            (p) => p.handle === seat.handle && p.lane === lane.key,
          )?.steps;
          return (
            <section
              key={`${seat.handle}:${lane.key}`}
              className="agent-access-review-seat"
              aria-label={`${seat.name} on ${lane.name}`}
            >
              <h3 className="t-label">
                {seat.name}
                {lanes.length > 1 ? ` on ${lane.name}` : ""}
              </h3>
              <ChangeList rows={rows(lane, from, to)} />
              {from.enabled && !to.enabled && <p>{laneWords(lane.key).off}</p>}
              <p className="t-caption">At {name}, the next sync will:</p>
              {steps ? (
                steps.length > 0 ? (
                  <Steps steps={steps} />
                ) : (
                  <p className="t-caption">Nothing. {name} already matches.</p>
                )
              ) : (
                <Spinner size="sm" />
              )}
            </section>
          );
        })}
      </div>
    </Modal>
  );
}

export function RemoveDialog({
  name,
  seat,
  lane,
  onClose,
  onRemove,
}: {
  name: string;
  seat: AccessSeat;
  lane: Lane;
  onClose: () => void;
  onRemove: () => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  return (
    <Modal
      open
      size="md"
      role="alertdialog"
      onClose={onClose}
      title={`Remove ${seat.name}'s ${lane.name} account?`}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="danger"
            loading={busy}
            onClick={async () => {
              setBusy(true);
              await onRemove();
            }}
          >
            Remove account
          </Button>
        </>
      }
    >
      <div className="agent-access-review">
        <p>{laneWords(lane.key).remove}</p>
        <p className="t-caption">
          Turning {seat.name} on at {name} again later creates a new account with a new credential.
        </p>
      </div>
    </Modal>
  );
}

export function PermissionsDialog({
  seat,
  lane,
  want,
  onChange,
  onClose,
}: {
  seat: AccessSeat;
  lane: Lane;
  want: AccessDesired;
  onChange: (key: string, access: "none" | GitHubAccess) => void;
  onClose: () => void;
}) {
  return (
    <Modal
      open
      size="md"
      onClose={onClose}
      title={`${seat.name}'s ${lane.name} permissions`}
      subtitle={levelsHeading(lane)}
      footer={
        <Button size="small" variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <DescriptionList
        uniform
        fit
        items={(lane.permissions ?? []).map((perm) => [
          perm.name,
          <SegmentedControl
            key={perm.key}
            size="sm"
            semantics="radio"
            label={`${seat.name}'s ${perm.name} permission`}
            value={want.permissions?.[perm.key] ?? "none"}
            onValueChange={(v) => onChange(perm.key, v as "none" | GitHubAccess)}
            options={(perm.write_only
              ? (["none", "write"] as const)
              : (["none", "read", "write"] as const)
            ).map((v) => ({ value: v, label: GITHUB_ACCESS_NAMES[v] }))}
          />,
        ])}
      />
    </Modal>
  );
}
