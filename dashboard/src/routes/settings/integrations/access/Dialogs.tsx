/**
 * The Agents tab's dialogs.
 *
 * - THE REVIEW, before a save: per agent, what the draft changes as one line
 *   of chips ("Deployments: Read-only → Read and write", in the vendor's
 *   words), what turning an agent off does there, and what the next sync will
 *   do at the vendor because of it: numbered steps, each a short label with
 *   its note beneath and the mark of who takes it, Crewlet's or a person's.
 *   The plan is the engine's (`POST .../access/plan`), so the preview and the
 *   sync cannot disagree. Where the plan says a save
 *   grants administration access, the review says the save asks for a fresh
 *   confirmation of who you are (D10); the transport asks for it when the
 *   engine refuses.
 * - THE REMOVAL of one agent's account, allowed only once it is off (D1).
 * - GITHUB'S PERMISSIONS for one agent, edited into the draft in GitHub's own
 *   words, so the row stays at one size.
 */

import { useEffect, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  ChangeList,
  DescriptionList,
  List,
  Modal,
  SegmentedControl,
  Spinner,
  Text,
  VisuallyHidden,
  type ChangeListRow,
} from "@crewlethq/ui";
import { CrewletIcon } from "@crewlethq/icons";
import { GLYPH_SIZES, UserGlyph } from "@crewlethq/icons/glyphs";
import type {
  AccessAnswer,
  AccessDesired,
  AccessPlanAnswer,
  AccessSeat,
  AccessStep,
  GitHubAccess,
} from "~/contract/access.ts";
import { accessApi, isStale, refusalWords, type AccessDraft } from "./api.ts";
import { lanesOf, levelName, levelsHeading, scopeApplies, scopeWords, type Lane } from "./logic.ts";
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
    if (scopeApplies(lane, to))
      out.push({
        kind: "fact",
        label: laneWords(lane.key).scope?.noun ?? "Scope",
        value: scopeWords(lane, to.scope),
      });
    return out;
  }
  if (!to.enabled) return out;
  if (lane.levels.length > 0 && (from.level ?? "") !== (to.level ?? "")) {
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
  if (scopeApplies(lane, to) && scopeWords(lane, from.scope) !== scopeWords(lane, to.scope)) {
    out.push({
      kind: "changed",
      label: laneWords(lane.key).scope?.noun ?? "Scope",
      from: scopeWords(lane, from.scope),
      to: scopeWords(lane, to.scope),
    });
  }
  return out;
}

/** Who takes a step, in words: what the mark beside it says to a screen reader. */
const ACTOR_WORDS = { crewlet: "Crewlet", you: "A person" } as const;

/** The one box both marks are drawn in, so a step's words start in one column. */
const ACTOR_SIZE = GLYPH_SIZES.md;

/**
 * The steps a sync takes, in order: each a short label with its note beneath
 * at a quieter step, and the mark of who takes it, Crewlet's own or a person.
 * The mark is said before the label, since a glyph is not a word.
 *
 * THE MARK SITS ON THE LABEL'S FIRST LINE, in a column of its own: both marks
 * are drawn in one box, a line tall, beside the words rather than in the row's
 * leading slot, which centres on the whole row and so set a step with a note
 * lower than one without.
 */
export function Steps({ steps, label }: { steps: AccessStep[]; label: string }) {
  return (
    <List variant="numbered" density="compact" aria-label={label}>
      {steps.map((s, i) => (
        <List.Item key={i}>
          <span className="agent-access-step">
            <span className="agent-access-actor">
              {s.by === "you" ? (
                <UserGlyph size={ACTOR_SIZE} aria-hidden />
              ) : (
                <CrewletIcon width={ACTOR_SIZE} height={ACTOR_SIZE} aria-hidden />
              )}
              <VisuallyHidden>{`${ACTOR_WORDS[s.by]}: `}</VisuallyHidden>
            </span>
            <span className="agent-access-step-words">
              <Text>{s.text}</Text>
              {s.note && (
                <Text variant="caption" tone="secondary">
                  {s.note}
                </Text>
              )}
            </span>
          </span>
        </List.Item>
      ))}
    </List>
  );
}

export function ReviewDialog({
  kind,
  name,
  data,
  draft,
  onClose,
  onSaved,
  onStale,
}: {
  kind: string;
  name: string;
  data: AccessAnswer;
  draft: AccessDraft;
  onClose: () => void;
  onSaved: (answer: AccessAnswer) => void;
  /** A save refused because somebody saved in between: read again, keeping the draft. */
  onStale: () => void;
}) {
  const [plan, setPlan] = useState<AccessPlanAnswer | null>(null);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  // PLANNED AGAIN FOR EVERY REVISION READ, so a review left open across a
  // colleague's save shows what the next sync does now.
  const revision = data.revision;
  useEffect(() => {
    let live = true;
    setPlan(null);
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
  }, [kind, draft, revision]);

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
      subtitle={`Nothing changes at ${name} until you save.`}
      footerStart={
        people ? (
          <span className="t-caption">Steps marked with a person need somebody at {name}.</span>
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
                onSaved(await accessApi.save(kind, draft, data.revision));
              } catch (err) {
                setError(refusalWords(err));
                setSaving(false);
                if (isStale(err)) onStale();
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
              <ChangeList layout="inline" rows={rows(lane, from, to)} />
              {from.enabled && !to.enabled && <p>{laneWords(lane.key).off}</p>}
              {steps ? (
                steps.length > 0 ? (
                  <Steps steps={steps} label={`What the next sync does at ${name}`} />
                ) : (
                  <p className="t-caption">Nothing to do. {name} already matches.</p>
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

/** A confirmation before one action on one seat's lane, saying what it does at the vendor. */
function SeatConfirm({
  title,
  confirm,
  danger,
  onClose,
  onConfirm,
  children,
}: {
  title: string;
  confirm: string;
  danger?: boolean;
  onClose: () => void;
  onConfirm: () => Promise<void>;
  children: ReactNode;
}) {
  const [busy, setBusy] = useState(false);
  return (
    <Modal
      open
      size="md"
      role="alertdialog"
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant={danger ? "danger" : "primary"}
            loading={busy}
            onClick={async () => {
              setBusy(true);
              await onConfirm();
            }}
          >
            {confirm}
          </Button>
        </>
      }
    >
      <div className="agent-access-review">{children}</div>
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
  return (
    <SeatConfirm
      title={`Remove ${seat.name}'s ${lane.name} account?`}
      confirm="Remove account"
      danger
      onClose={onClose}
      onConfirm={onRemove}
    >
      <p>{laneWords(lane.key).remove}</p>
      <p className="t-caption">
        Turning {seat.name} on at {name} again later creates a new account with a new credential.
      </p>
    </SeatConfirm>
  );
}

/** Rotation, said before it runs: what the vendor does, and what stops working. */
export function RotateDialog({
  seat,
  lane,
  onClose,
  onRotate,
}: {
  seat: AccessSeat;
  lane: Lane;
  onClose: () => void;
  onRotate: () => Promise<void>;
}) {
  return (
    <SeatConfirm
      title={`Rotate ${seat.name}'s ${lane.name} credential?`}
      confirm="Rotate credential"
      onClose={onClose}
      onConfirm={onRotate}
    >
      <p>{laneWords(lane.key).rotate}</p>
    </SeatConfirm>
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
