/**
 * The schedule's own controls: New schedule, Edit, Pause and Resume, Delete,
 * and Run now.
 *
 * TWO KINDS OF CHANGE, decided by two gates. The first four rewrite the
 * company document (`scheduleWrite.ts`): a schedule is a line of the seat or
 * team that declares it, so they are the configuration's to allow, which is
 * Admin or a lead inside their own team ([useOrgWriteAccess]) on a document
 * nobody else manages. Run now changes nothing in the document: it is the
 * engine's act `run_schedule` (ADR-0024), which fires the schedule once now,
 * off its cron, and is decided on every seat it wakes, as pausing a seat is,
 * so a reader is held where they do not lead every one of them
 * ([runHold]).
 *
 * EVERY CONTROL IS DRAWN, disabled with the reason it cannot act
 * (`lib/useWriteAccess.ts`), never hidden.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  ConfirmModal,
  FormField,
  FormRow,
  Input,
  ReadOnlyField,
  SegmentedControl,
  Select,
  Switch,
  Text,
  Textarea,
  useToast,
  type SelectOption,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import {
  PauseGlyph,
  PencilGlyph,
  PlusGlyph,
  RotateCwGlyph,
  TrashGlyph,
  ZapGlyph,
} from "@crewlethq/icons/glyphs";
import { WriteButton } from "~/components/WriteButton.tsx";
import { describe as describeCron } from "~/lib/cron.ts";
import { fmtSetting } from "~/lib/format.ts";
import { useAct } from "~/lib/useAct.ts";
import { useLastHealth } from "~/lib/useLastHealth.ts";
import { nodeStanding, type ActivatedRevision, type Standing } from "~/lib/nodeStanding.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { handleLabel, indexOrg, type OrgIndex } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  mayActOnSeat,
  useOrgWriteAccess,
  useOrgWriteAccessFor,
  type ConfigWriteAccess,
  type OrgTarget,
} from "~/lib/useWriteAccess.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { getEntity } from "~/protocol/configWrite.ts";
import { warningWords } from "~/protocol/configAnswer.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { ScheduleRow } from "~/protocol/index.ts";
import {
  NEW_DRAFT,
  draftOf,
  draftProblems,
  entityPath,
  ownerOf,
  ownerWords,
  scheduleRefusalWords,
  writtenScheduleOf,
  type DraftField,
  type ScheduleDraft,
  type ScheduleOwner,
  type ScheduleScope,
  type ScheduleTarget,
} from "./scheduleWrite.ts";
import { useScheduleWrite, type ScheduleWrite } from "./useScheduleWrite.ts";

/** The org target an owner's configuration write is decided on. */
function targetOf(owner: ScheduleOwner): OrgTarget {
  return owner.scope === "role" ? { seat: owner.id } : { unit: owner.id };
}

/** What a saved change says, once, as a toast. */
const SAVED_MESSAGE =
  "Saved to the company’s configuration. The schedule changes as soon as the engine applies it.";

/**
 * Why Run now is held for this reader on this schedule, once the act itself is
 * theirs (`WriteButton` says the act's own reasons, offline, signed out, not
 * offered, first), or undefined where it is not: the schedule's own: one switched off does not run by hand either (the engine
 * refuses it), one nothing can run has nobody to wake, and a reader who may
 * not wake every seat it reaches is told who may.
 */
export function runHold(
  row: Pick<ScheduleRow, "enabled" | "runners">,
  mayWake: (handle: string) => boolean,
): string | undefined {
  if (!row.enabled) return "Paused. Resume it to run it.";
  if ((row.runners ?? []).length === 0) return "Nobody can run this schedule.";
  if (!(row.runners ?? []).every(mayWake)) {
    return "Running it takes Admin access, or leading every seat it wakes.";
  }
  return undefined;
}

/** A saved change followed until this node applies it, and the switch it set, if it set one. */
interface Followed {
  readonly activated: ActivatedRevision;
  /** What a pause (false) or a resume (true) saved; absent for any other change. */
  readonly enabled?: boolean;
}

/**
 * Following a saved schedule change until THIS node applies it, and asking for
 * the schedules to be read again then.
 *
 * WHAT THE SCREEN DRAWS IS THIS NODE'S APPLIED COMPANY, read every thirty
 * seconds, and a change saved here lands in it only once the node applies the
 * revision. A save that stopped at "saved" left the page drawing the schedule
 * as it was for up to half a minute: a pause just saved still offered Run now
 * (which the engine then refused, the schedule being switched off) and Pause,
 * and still listed the runs ahead. The standing is this node's own word on the
 * revision (`lib/nodeStanding.ts`), by its last health push, as every other
 * follower of a configuration write reads it.
 */
function useFollowedSave(onApplied?: () => void): {
  readonly followed: Followed | null;
  readonly standing: Standing["kind"] | null;
  readonly follow: (next: Followed | null) => void;
} {
  const [followed, follow] = useState<Followed | null>(null);
  const health = useLastHealth();
  const standing =
    followed && health !== null ? nodeStanding(health, followed.activated).kind : null;
  const told = useRef<ActivatedRevision | null>(null);
  const appliedRef = useRef(onApplied);
  appliedRef.current = onApplied;
  useEffect(() => {
    if (!followed || (standing !== "applied" && standing !== "passed")) return;
    // ONCE PER SAVE: the re-read is asked for when the node applies it.
    if (told.current === followed.activated) return;
    told.current = followed.activated;
    appliedRef.current?.();
  }, [followed, standing]);
  return { followed, standing, follow };
}

/**
 * Whether the schedule is switched on, as its controls decide it: the switch a
 * pause or resume just saved, until the schedule as read agrees with it or is
 * read again after this node applied it; the row's own otherwise. A node that
 * REFUSED the revision runs the schedule as it was, so the row is the truth
 * there.
 */
function useDrawnSwitch(
  row: ScheduleRow,
  { followed, standing, follow }: ReturnType<typeof useFollowedSave>,
): boolean {
  const asked = followed?.enabled;
  // The row this node's apply found on screen; a newer one is the re-read.
  const seen = useRef<ScheduleRow | null>(null);
  useEffect(() => {
    if (asked === undefined) {
      seen.current = null;
      return;
    }
    if (row.enabled === asked || standing === "refused") {
      seen.current = null;
      follow(null);
      return;
    }
    if (standing !== "applied" && standing !== "passed") return;
    if (seen.current === null) seen.current = row;
    else if (row !== seen.current) {
      seen.current = null;
      follow(null);
    }
  }, [row, asked, standing, follow]);
  return asked !== undefined && standing !== "refused" ? asked : row.enabled;
}

/** The controls for one schedule, for its page's bar and its peek's header. */
export function ScheduleActions({
  row,
  compact = false,
  onApplied,
}: {
  row: ScheduleRow;
  compact?: boolean;
  /** Read the schedules again: called once this node applies a change saved here. */
  onApplied?: () => void;
}) {
  const owner = useMemo(() => ownerOf(row), [row]);
  const access = useOrgWriteAccess(targetOf(owner));
  const toast = useToast();
  const saves = useFollowedSave(onApplied);
  const { follow } = saves;
  // THE SWITCH ASKED FOR, kept beside the write: the save's answer says what
  // was stored, and the switch is what the controls draw until it lands.
  const asking = useRef<boolean | null>(null);
  const write = useScheduleWrite(owner, {
    onSaved: (done, activated) => {
      toast.show({ variant: "success", title: done, message: SAVED_MESSAGE });
      follow({ activated, ...(asking.current === null ? {} : { enabled: asking.current }) });
    },
  });
  const followed = useCallback((activated: ActivatedRevision) => follow({ activated }), [follow]);
  const enabled = useDrawnSwitch(row, saves);
  const run = useAct("run_schedule");
  const viewer = useViewer();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const [editing, setEditing] = useState(false);
  const [removing, setRemoving] = useState(false);
  const held = access.can ? undefined : access.reason;
  // HELD ON THE SWITCH AS SAVED: a pause just saved switched the schedule off
  // for the engine, which refuses a run of it by hand, whatever this node's
  // last read still says.
  const runBlocked = run.access.can
    ? runHold({ ...row, enabled }, (handle) => mayActOnSeat(viewer, index, handle))
    : undefined;
  const size = "small";

  const runNow = () => {
    void run.run(
      { scope_type: row.scope_type, scope: owner.id, name: row.name },
      {
        done: `Ran “${row.name}” now`,
        landed: (receipt) => ranNow(receipt),
      },
    );
  };

  return (
    <>
      <WriteButton
        write={run}
        size={size}
        variant="primary"
        leadingIcon={<ZapGlyph size="sm" />}
        {...(runBlocked === undefined ? {} : { blocked: runBlocked })}
        onPress={runNow}
      >
        Run now
      </WriteButton>
      <span className={compact ? undefined : "page-action-folds"}>
        <Button
          size={size}
          variant="secondary"
          leadingIcon={<PencilGlyph size="sm" />}
          disabledReason={held}
          onClick={() => setEditing(true)}
        >
          Edit
        </Button>
      </span>
      <span className={compact ? undefined : "page-action-folds"}>
        <Button
          size={size}
          variant="secondary"
          leadingIcon={enabled ? <PauseGlyph size="sm" /> : <RotateCwGlyph size="sm" />}
          loading={write.busy && !editing && !removing}
          disabledReason={held}
          title={
            enabled
              ? "Stops it firing until it is resumed. It stays in the configuration."
              : "Fires it on its schedule again."
          }
          onClick={() => {
            asking.current = !enabled;
            write.submit({ kind: "enable", name: row.name, enabled: !enabled });
          }}
        >
          {enabled ? "Pause" : "Resume"}
        </Button>
      </span>
      <span className={compact ? undefined : "page-action-folds"}>
        <Button
          size={size}
          variant="ghost"
          leadingIcon={<TrashGlyph size="sm" />}
          disabledReason={held}
          onClick={() => setRemoving(true)}
        >
          Delete
        </Button>
      </span>
      {!editing && !removing && <WriteOutcome write={write} />}
      {editing && (
        <ScheduleDialog
          mode={{ kind: "edit", row }}
          onClose={() => setEditing(false)}
          onSaved={followed}
        />
      )}
      {removing && (
        <RemoveDialog
          row={row}
          owner={owner}
          access={access}
          onClose={() => setRemoving(false)}
          onSaved={followed}
        />
      )}
    </>
  );
}

/** What a run asked for came to, from the act's receipt: each seat's word. */
function ranNow(receipt: unknown): string | undefined {
  const runs = (receipt as { runs?: { handle?: string; status?: string }[] } | null)?.runs;
  if (!Array.isArray(runs)) return undefined;
  const by = (status: string) =>
    runs.filter((r) => r.status === status).map((r) => handleLabel(r.handle));
  const sent = [...by("fired"), ...by("already_fired")];
  const paused = by("paused");
  const waiting = [...by("not_ready"), ...by("unknown")];
  const parts = [sent.length ? `Sent to ${list(sent)}.` : ""];
  if (paused.length)
    parts.push(`${list(paused)} ${paused.length === 1 ? "is" : "are"} paused and was not woken.`);
  if (waiting.length)
    parts.push(`Not confirmed for ${list(waiting)}: its runs say whether it arrived.`);
  return parts.filter(Boolean).join(" ");
}

function list(items: readonly string[]): string {
  return items.length <= 1
    ? (items[0] ?? "")
    : `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`;
}

/**
 * A pause, resume or removal that did not save, said under the controls; a
 * saved one is the toast's.
 */
function WriteOutcome({ write }: { write: ScheduleWrite }) {
  const { state } = write;
  if (state.kind === "refused") {
    return (
      <Callout variant="danger" title="Couldn’t save that" role="alert">
        {state.message}
      </Callout>
    );
  }
  if (state.kind === "problems") {
    return (
      <Callout variant="danger" title="The engine refused the change" role="alert">
        {[...Object.values(state.fields), ...state.general].join(" ")}
      </Callout>
    );
  }
  if (state.kind === "warned") {
    return (
      <Callout
        variant="warning"
        title="The engine would also say"
        action={
          <Button size="small" variant="secondary" onClick={write.confirm}>
            Save anyway
          </Button>
        }
      >
        {state.warnings.map(warningWords).join(" ")}
      </Callout>
    );
  }
  return null;
}

/** Removing a schedule: what it ends, asked once. */
function RemoveDialog({
  row,
  owner,
  access,
  onClose,
  onSaved,
}: {
  row: ScheduleRow;
  owner: ScheduleOwner;
  access: ConfigWriteAccess;
  onClose: () => void;
  /** Told the revision the removal activated, for the screen to follow it. */
  onSaved: (activated: ActivatedRevision) => void;
}) {
  const toast = useToast();
  const write = useScheduleWrite(owner, {
    onSaved: (done, activated) => {
      toast.show({ variant: "success", title: done, message: SAVED_MESSAGE });
      onSaved(activated);
      onClose();
    },
  });
  return (
    <ConfirmModal
      open
      onClose={onClose}
      destructive
      title={`Delete “${row.name}”?`}
      message={`It stops firing for ${ownerWords(owner)} and leaves the company’s configuration. Its runs so far stay in its history.`}
      confirmLabel="Delete"
      submitting={write.busy}
      confirmDisabledReason={access.can ? undefined : access.reason}
      onConfirm={() => write.submit({ kind: "remove", name: row.name })}
    >
      <WriteOutcome write={write} />
    </ConfirmModal>
  );
}

/** The New schedule button, for the list's bar and its empty state. */
export function NewScheduleButton({
  variant = "primary",
  onApplied,
}: {
  variant?: "primary" | "secondary";
  /** Read the schedules again: called once this node applies the schedule added here. */
  onApplied?: () => void;
}) {
  const access = useOrgWriteAccess("anywhere");
  const [open, setOpen] = useState(false);
  const { follow } = useFollowedSave(onApplied);
  const followed = useCallback((activated: ActivatedRevision) => follow({ activated }), [follow]);
  return (
    <>
      <Button
        size="small"
        variant={variant}
        leadingIcon={<PlusGlyph size="sm" />}
        disabledReason={access.can ? undefined : access.reason}
        onClick={() => setOpen(true)}
      >
        New schedule
      </Button>
      {open && (
        <ScheduleDialog
          mode={{ kind: "create" }}
          onClose={() => setOpen(false)}
          onSaved={followed}
        />
      )}
    </>
  );
}

type DialogMode = { kind: "create" } | { kind: "edit"; row: ScheduleRow };

/** The seats and teams a new schedule can be written on, as a picker offers them. */
function ownerOptions(index: OrgIndex, scope: ScheduleScope): SelectOption[] {
  return scope === "role"
    ? index.seats
        .filter((s) => s.kind === "agent")
        .map((s) => ({ value: s.handle, label: s.name, hint: handleLabel(s.handle) }))
    : index.units.map((u) => ({ value: u.id, label: u.name, hint: u.id }));
}

/** Every zone this browser knows, the company's clock first. */
function zoneOptions(companyZone: string, current: string): SelectOption[] {
  const known =
    typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : [];
  const zones = current && !known.includes(current) ? [current, ...known] : known;
  return [
    { value: "", label: `The company’s clock (${companyZone || "UTC"})` },
    ...zones.map((z) => ({ value: z, label: z })),
  ];
}

/**
 * A new schedule, or an existing one's edit, checked by the engine before it
 * is saved.
 */
/**
 * What an empty time limit means, from the engine's own default: a copy of
 * the constant here would be a second answer that drifts.
 */
export function emptyCapWords(seconds: number | undefined): string {
  if (seconds === undefined || seconds <= 0) return "Empty is the engine’s default.";
  return `Empty is the engine’s ${fmtSetting(seconds)}.`;
}

export function ScheduleDialog({
  mode,
  onClose,
  onSaved,
}: {
  mode: DialogMode;
  onClose: () => void;
  /** Told the revision the save activated, for the screen behind the dialog to follow it. */
  onSaved?: (activated: ActivatedRevision) => void;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const companyZone = org?.timezone ?? "";
  const accessFor = useOrgWriteAccessFor();
  const toast = useToast();
  const editing = mode.kind === "edit" ? mode.row : null;
  const [scope, setScope] = useState<ScheduleScope>(
    editing ? (editing.scope_type === "role" ? "role" : "unit") : "unit",
  );
  const [ownerId, setOwnerId] = useState(editing ? ownerOf(editing).id : "");
  const owner: ScheduleOwner | null = ownerId ? { scope, id: ownerId } : null;
  const [draft, setDraft] = useState<ScheduleDraft>(NEW_DRAFT);
  const [touched, setTouched] = useState(false);
  // AN EDIT OPENS ON THE SCHEDULE AS THE DOCUMENT WRITES IT, never on the row
  // the screen drew: the row is the RESOLVED schedule (its zone filled in
  // from the company's, its cap from the engine's default), and a form
  // opened on it would write both into a schedule that left them to the
  // company and the engine, as changes nobody made.
  const [reading, setReading] = useState<"reading" | "ready" | { refused: string }>(
    editing ? "reading" : "ready",
  );
  const editKey = editing
    ? `${editing.scope_type}\u0000${editing.scope_id}\u0000${editing.name}`
    : "";
  useEffect(() => {
    if (!editing) return;
    const controller = new AbortController();
    const at = ownerOf(editing);
    void getEntity(entityPath(at), at.id, controller.signal).then(
      (read) => {
        if (read.kind !== "entity") {
          setReading({ refused: scheduleRefusalWords(read, at).message });
          return;
        }
        const written = writtenScheduleOf(read.entity, editing.name);
        if (!written) {
          setReading({
            refused: `“${editing.name}” is no longer on ${ownerWords(at)} in the company’s configuration.`,
          });
          return;
        }
        setDraft(draftOf(written));
        setReading("ready");
      },
      (err: unknown) => {
        if (!isAbort(err)) {
          setReading({
            refused: err instanceof Error ? err.message : "The engine could not be reached.",
          });
        }
      },
    );
    return () => controller.abort();
    // KEYED ON THE SCHEDULE'S IDENTITY, not its row: the list re-reads every
    // thirty seconds, and a new row object for the same schedule must not
    // throw away what the person has typed.
  }, [editKey]);
  const write = useScheduleWrite(owner, {
    onSaved: (done, activated) => {
      toast.show({ variant: "success", title: done, message: SAVED_MESSAGE });
      onSaved?.(activated);
      onClose();
    },
  });
  const set = <K extends keyof ScheduleDraft>(key: K, value: ScheduleDraft[K]) => {
    setDraft((d) => ({ ...d, [key]: value }));
    if (write.state.kind !== "idle" && !write.busy) write.reset();
  };

  // THE NAMES ALREADY TAKEN where the schedule would land, from the
  // company's schedules: the engine refuses a duplicate too, this says so
  // before anything is sent.
  // ALSO WHAT AN EMPTY TIME LIMIT MEANS, the engine's own default, read on
  // an edit too.
  const listed = useQuery("schedules", undefined);
  const defaultCap = listed.data?.default_timeout_seconds;
  const taken = useMemo(() => {
    if (!owner || editing) return [];
    return (listed.data?.schedules ?? [])
      .filter((row) => {
        const at = ownerOf(row);
        return at.scope === owner.scope && at.id === owner.id;
      })
      .map((row) => row.name);
  }, [owner, editing, listed.data]);
  const local = draftProblems(draft, taken);
  const engine = write.state.kind === "problems" ? write.state.fields : {};
  const problem = (field: DraftField): string | undefined =>
    engine[field] ?? (touched ? local[field] : undefined);

  const access: ConfigWriteAccess = owner ? accessFor(targetOf(owner)) : accessFor("anywhere");
  const blocked = !access.can
    ? access.reason
    : !owner
      ? `Choose the ${scope === "role" ? "seat" : "team"} it belongs to.`
      : reading === "reading"
        ? "Reading the schedule…"
        : typeof reading === "object"
          ? reading.refused
          : undefined;

  const save = () => {
    setTouched(true);
    if (blocked || Object.keys(local).length > 0 || !owner) return;
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    write.submit(editing ? { kind: "edit", name: editing.name, draft } : { kind: "create", draft });
  };

  const said = describeCron(draft.cron.trim());
  const scopeOptions = [
    { value: "unit" as const, label: "A team" },
    { value: "role" as const, label: "A seat" },
  ];

  return (
    <Modal
      open
      onClose={onClose}
      size="md"
      stackBody
      title={editing ? `Edit “${editing.name}”` : "New schedule"}
      subtitle={
        editing
          ? `On ${ownerWords(ownerOf(editing))}`
          : "Recurring work for a seat or a team, on a cron."
      }
      onSubmit={save}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" loading={write.busy} disabledReason={blocked}>
            {write.state.kind === "warned" ? "Save anyway" : editing ? "Save" : "Add schedule"}
          </Button>
        </>
      }
    >
      {editing ? (
        // THE NAME IS FIXED ONCE THE SCHEDULE EXISTS: its runs, its history
        // and its conversation on each seat are filed under it.
        <ReadOnlyField
          label="Name"
          value={editing.name}
          mono
          reason="A schedule’s runs and its conversation are kept under its name, so it cannot be renamed. Add a new schedule to use another name."
        />
      ) : (
        <>
          <FormField label="Belongs to" as="fieldset">
            <SegmentedControl
              label="Belongs to"
              semantics="radio"
              options={scopeOptions}
              value={scope}
              onValueChange={(next: ScheduleScope) => {
                setScope(next);
                setOwnerId("");
              }}
            />
          </FormField>
          <FormField
            label={scope === "role" ? "Seat" : "Team"}
            htmlFor="schedule-owner"
            helper={
              scope === "role"
                ? "The agent seat that runs it."
                : "Who in the team runs it is chosen below."
            }
          >
            <Select
              id="schedule-owner"
              ariaLabel={scope === "role" ? "Seat" : "Team"}
              searchable
              placeholder={scope === "role" ? "Choose a seat" : "Choose a team"}
              value={ownerId || undefined}
              options={ownerOptions(index, scope)}
              onChange={(next) => setOwnerId(String(next))}
            />
          </FormField>
          <FormField label="Name" htmlFor="schedule-name" error={problem("name")}>
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                appearance="reference"
                error={Boolean(problem("name"))}
                value={draft.name}
                placeholder="daily-standup"
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => set("name", e.target.value)}
              />
            )}
          </FormField>
        </>
      )}
      <FormRow columns={2}>
        <FormField
          label="When"
          htmlFor="schedule-cron"
          {...(problem("cron")
            ? { error: problem("cron") }
            : { helper: said ?? "Five fields: minute hour day month weekday." })}
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              appearance="reference"
              error={Boolean(problem("cron"))}
              value={draft.cron}
              placeholder="30 9 * * 1-5"
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => set("cron", e.target.value)}
            />
          )}
        </FormField>
        <FormField
          label="Time zone"
          htmlFor="schedule-zone"
          {...(problem("timezone")
            ? { error: problem("timezone") }
            : {
                helper: draft.timezone
                  ? "Its times are read on this clock."
                  : "Follows the company’s clock if that changes.",
              })}
        >
          <Select
            id="schedule-zone"
            ariaLabel="Time zone"
            searchable
            value={draft.timezone}
            options={zoneOptions(companyZone, draft.timezone)}
            onChange={(next) => set("timezone", String(next))}
          />
        </FormField>
      </FormRow>
      <FormField label="Task" htmlFor="schedule-task" error={problem("task")}>
        {(field) => (
          <Textarea
            id={field.id}
            aria-describedby={field.describedBy}
            error={Boolean(problem("task"))}
            value={draft.task}
            rows={3}
            autoResize
            placeholder="Post your standup: shipped yesterday, on today, blockers."
            onChange={(e) => set("task", e.target.value)}
          />
        )}
      </FormField>
      {scope === "unit" && (
        <FormField label="Who runs it" error={problem("target")} as="fieldset">
          <SegmentedControl<ScheduleTarget>
            label="Who runs it"
            semantics="radio"
            options={[
              { value: "each", label: "Each agent in the team" },
              { value: "lead", label: "The team’s lead" },
            ]}
            value={draft.target}
            onValueChange={(next) => set("target", next)}
          />
        </FormField>
      )}
      <FormField
        label="Time limit, in minutes"
        htmlFor="schedule-timeout"
        optional
        {...(problem("timeout")
          ? { error: problem("timeout") }
          : { helper: `${emptyCapWords(defaultCap)} A run past it is stopped.` })}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            width="sm"
            inputMode="decimal"
            error={Boolean(problem("timeout"))}
            value={draft.timeoutMinutes}
            onChange={(e) => set("timeoutMinutes", e.target.value)}
          />
        )}
      </FormField>
      <Switch
        label="Catch up after a restart"
        description="Runs the most recent missed time once, if it was recent enough."
        checked={draft.catchup}
        onCheckedChange={(on) => set("catchup", on)}
      />
      <Switch
        label="Running"
        description="Off pauses it: it stays here and does not fire until it is resumed."
        checked={draft.enabled}
        onCheckedChange={(on) => set("enabled", on)}
      />
      {typeof reading === "object" && (
        <Callout variant="danger" title="Couldn’t read this schedule" role="alert">
          {reading.refused}
        </Callout>
      )}
      <DialogOutcome write={write} />
    </Modal>
  );
}

/** A refusal or a warning the dialog's last check or save came back with. */
function DialogOutcome({ write }: { write: ScheduleWrite }): ReactNode {
  const { state } = write;
  if (state.kind === "checking") return <Text as="p">Checking the change with the engine…</Text>;
  if (state.kind === "unchanged")
    return <Text as="p">Nothing changed, so there is nothing to save.</Text>;
  if (state.kind === "problems" && state.general.length > 0) {
    return (
      <Callout variant="danger" title="The engine refused the change" role="alert">
        {state.general.join(" ")}
      </Callout>
    );
  }
  if (state.kind === "warned") {
    return (
      <Callout variant="warning" title="The engine would also say">
        {state.warnings.map(warningWords).join(" ")}
      </Callout>
    );
  }
  if (state.kind === "refused") {
    return (
      <Callout variant="danger" title="Couldn’t save that" role="alert">
        {state.message}
      </Callout>
    );
  }
  return null;
}
