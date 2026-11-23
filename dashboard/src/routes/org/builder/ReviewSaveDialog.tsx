/**
 * Review and save: what the save changes, what follows from it, and the
 * write itself.
 *
 * CONSEQUENCES BEFORE THE BUTTON. A rename re-onboards seats, a reorder can
 * change who a seat reports to, a removal clears references and leaves vendor
 * accounts behind. `model/changes.ts` derives all of it from the base and the
 * draft as the engine derived them, and this dialog draws each value that
 * moves as a row on its seat or unit (`ChangeGroups`) and the rest as facts
 * (`ConsequenceList`). The consequences that cannot be taken back (a company
 * rename, a change of tool credentials, removing most of the company) are
 * required choices there, which hold Save until each is ticked.
 *
 * THE WARNINGS ARE THE ENGINE'S, for exactly the body the save will send: the
 * check of the current draft is a dry run of that same request. They are the
 * lines the banner over the chart draws ([draftWarningLines]), so the two say
 * the same thing and count it the same way.
 *
 * THE AUDIT SUMMARY CARRIES THE WRITE ID. The operator's sentence is recorded
 * with the revision, followed by the write id, which is what lets the builder
 * recognize this save if its answer is lost (`useSave.ts`). It is the builder's
 * own mark on the record, so the form asks for the sentence and shows no id.
 */

import { engineWords } from "~/components/ConfigWarnings.tsx";
import { useState } from "react";
import { plural } from "~/lib/format.ts";
import { handleLabel } from "~/lib/seats.ts";
import { ConfigField } from "~/components/ConfigField.tsx";
import { acknowledgementChoice, BuilderWarnings } from "./dialogParts.tsx";
import type { Acknowledgement, ChangeSet, EntityRef, OnboardingCause } from "./model/changes.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import type { PlacedProblem } from "./model/problems.ts";
import type { DraftWarnings } from "./model/warnings.ts";
import type { CheckStatus, SaveRules } from "./model/scheduler.ts";
import type { BuilderMode } from "./model/transport.ts";
import type { SavePhase } from "./useSave.ts";
import { SaveGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  Callout,
  ChangeGroups,
  ConsequenceList,
  consequencesAnswered,
  Eyebrow,
  NoticeList,
  Spinner,
  type ChangeGroup,
  type ChangeListRow,
  type ConsequenceFact,
  type ConsequenceItem,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";

/** Why a group of seats onboards again, agreeing with how many there are. */
const ONBOARDING_CAUSE: Record<OnboardingCause, (one: boolean) => string> = {
  company_rename: () => "because the company is renamed",
  seat_rename: (one) => (one ? "because it is renamed" : "because they are renamed"),
  unit_rename: (one) =>
    one ? "because a unit it belongs to is renamed" : "because a unit they belong to is renamed",
  move: (one) => (one ? "because it moves to another unit" : "because they move to another unit"),
};

const names = (refs: readonly EntityRef[]) => refs.map((r) => r.name).join(", ");
const place = (ref: EntityRef) => (ref.kind === "company" ? "Top level" : ref.name);
const who = (ref: EntityRef | null) => (ref ? ref.name : "Nobody");
const inherited = (flag: boolean) => (flag ? " (inherited)" : "");
const KIND_WORD: Record<EntityRef["kind"], string> = {
  seat: "Seat",
  unit: "Unit",
  company: "Company",
};
const REFERENCE_WORD: Record<string, string> = {
  lead: "Lead",
  manages: "Manages",
  unit: "Unit reference",
  gitlab_access_level: "GitLab access level",
};

/** A value that may be none, as a changed, added or removed row. */
function pair(
  label: string,
  before: string | null,
  after: string | null,
  note?: string,
): ChangeListRow {
  const tail = note === undefined ? {} : { note };
  if (before === null) return { kind: "added", label, to: after ?? "None", ...tail };
  if (after === null) return { kind: "removed", label, from: before, ...tail };
  return { kind: "changed", label, from: before, to: after, ...tail };
}

/** Rows gathered per subject, in the order subjects first appear. */
function subjects() {
  const groups = new Map<string, { key: string; subject: string; rows: ChangeListRow[] }>();
  return {
    add(key: string, subject: string, row: ChangeListRow) {
      const group = groups.get(key) ?? { key, subject, rows: [] };
      group.rows.push(row);
      groups.set(key, group);
    },
    list: (): ChangeGroup[] => [...groups.values()],
  };
}

/**
 * Every change a subject at a time, and what follows as facts.
 *
 * A VALUE THAT MOVES FROM ONE THING TO ANOTHER IS A ROW, on the seat or unit
 * it belongs to, whether the draft set it or the engine derives it.
 */
export function reviewParts(changes: ChangeSet): {
  changes: ChangeGroup[];
  follows: ConsequenceFact[];
} {
  const out = subjects();
  const on = (ref: EntityRef, row: ChangeListRow) => out.add(ref.key, ref.name, row);
  const company = (row: ChangeListRow) => out.add(COMPANY_KEY, "Company", row);
  if (changes.companyRename) {
    company(pair("Name", changes.companyRename.before, changes.companyRename.after));
  }
  for (const r of changes.added) on(r, { kind: "added", label: KIND_WORD[r.kind], to: r.name });
  for (const r of changes.removed)
    on(r, { kind: "removed", label: KIND_WORD[r.kind], from: r.name });
  for (const r of changes.renamed) on(r.ref, pair("Name", r.before, r.after));
  for (const m of changes.moved) on(m.ref, pair("Unit", place(m.from), place(m.to)));
  for (const e of changes.edited)
    on(e.ref, { kind: "fact", label: "Edited", value: e.fields.join(", ") });
  for (const r of changes.reportsTo) {
    on(r.ref, pair(r.orderOnly ? "Reports first to" : "Reports to", who(r.before), who(r.after)));
  }
  for (const l of changes.leads) {
    on(
      l.ref,
      pair(
        "Lead",
        `${who(l.before)}${inherited(l.beforeInherited)}`,
        `${who(l.after)}${inherited(l.afterInherited)}`,
      ),
    );
  }
  for (const c of changes.channels) {
    on(
      c.ref,
      pair(
        "Channel",
        `${c.before || "None"}${inherited(c.beforeInherited)}`,
        `${c.after || "None"}${inherited(c.afterInherited)}`,
      ),
    );
  }
  for (const r of changes.routing) {
    const container =
      r.field === "project"
        ? "Unrouted work in tracker project"
        : "Unrouted work in knowledge space";
    on(
      r.holder,
      pair(
        `${container} ${r.scope}`,
        who(r.before),
        who(r.after),
        r.shared ? "Declared by more than one owner." : undefined,
      ),
    );
  }
  for (const s of changes.credentialServers) {
    if (s.gained.length > 0) on(s.ref, pair("Tool credentials", null, s.gained.join(", ")));
    if (s.lost.length > 0) on(s.ref, pair("Tool credentials", s.lost.join(", "), null));
  }
  for (const c of changes.clearedReferences) {
    on(c.holder, pair(REFERENCE_WORD[c.kind] ?? c.kind, c.from, null));
  }
  if (changes.datadogFallback) {
    const { before, after } = changes.datadogFallback;
    company(pair("Datadog fallback", before ? `@${before}` : null, after ? `@${after}` : null));
  }
  for (const g of changes.gitlabAccessLevels) {
    out.add(`@${g.handle}`, `@${g.handle}`, pair("GitLab access level", g.before, g.after));
  }

  const follows: ConsequenceFact[] = [];
  const fact = (key: string, text: string, tone: ConsequenceFact["tone"] = "follows") =>
    follows.push({ kind: "fact", key, text, tone });
  for (const group of changes.onboarding) {
    const one = group.seats.length === 1;
    fact(
      `onboard-${group.cause}`,
      `${plural(group.seats.length, "seat")} ${one ? "onboards" : "onboard"} again ${ONBOARDING_CAUSE[group.cause](one)}: ${names(group.seats)}.`,
    );
  }
  for (const u of changes.unitRenames) {
    fact(
      `pages-${u.ref.key}`,
      `Onboarding pages for ${u.before} are looked up under its new name, ${u.after}.`,
    );
  }
  for (const u of changes.uncarriedCredentials) {
    fact(
      `uncarried-${u.ref.key}`,
      `${u.ref.name} starts without ${handleLabel(u.from)}'s credentials ${u.fields.join(", ")}: the dashboard never holds their values, so they do not carry over.`,
      "warning",
    );
  }
  return { changes: out.list(), follows };
}

export function ReviewSaveDialog({
  mode,
  changes,
  rules,
  status,
  warnings,
  problemCount,
  documentProblems,
  phase,
  onSave,
  onCheckAgain,
  onClose,
}: {
  mode: BuilderMode;
  changes: ChangeSet;
  rules: SaveRules;
  status: CheckStatus;
  /** The current check's warnings: a dry run of exactly this save. */
  warnings: DraftWarnings;
  problemCount: number;
  documentProblems: readonly PlacedProblem[];
  phase: SavePhase;
  onSave: (summary: string) => void;
  onCheckAgain: () => void;
  onClose: () => void;
}) {
  const [summary, setSummary] = useState(changes.summary);
  const [acknowledged, setAcknowledged] = useState<ReadonlySet<Acknowledgement>>(new Set());
  const busy = phase.kind === "saving" || phase.kind === "settling";
  const unknown = phase.kind === "unknown";
  const parts = reviewParts(changes);
  // SAYS WHETHER A CHECK IS RUNNING: "until the check finishes" read the same
  // with one out and with none coming.
  const pending: ConsequenceFact[] = changes.derivedKnown
    ? []
    : [
        status === "checking"
          ? {
              kind: "fact",
              key: "pending",
              icon: <Spinner size="xs" />,
              text: "Checking what else this changes.",
            }
          : {
              kind: "fact",
              key: "pending",
              tone: "info",
              text: "The check hasn’t answered, so what else this changes may not be listed.",
            },
      ];
  const follows = [...pending, ...parts.follows];
  const acknowledgements: ConsequenceItem[] = changes.acknowledgements.map((a) =>
    acknowledgementChoice(a, {
      checked: acknowledged.has(a),
      disabled: busy,
      massRemoval: changes.massRemoval,
      onCheckedChange: (checked) =>
        setAcknowledged((current) => {
          const next = new Set(current);
          if (checked) next.add(a);
          else next.delete(a);
          return next;
        }),
    }),
  );
  const allAcknowledged = consequencesAnswered(acknowledgements);
  const summaryMissing = summary.trim() === "";
  const canSave = rules.save && allAcknowledged && !summaryMissing && !busy;
  const create = mode === "create";

  const saveLabel = busy
    ? phase.kind === "settling"
      ? "Checking the save"
      : "Saving"
    : rules.waiting
      ? "Waiting for the check"
      : unknown
        ? "Save again"
        : create
          ? "Create the company"
          : "Save";

  return (
    <Modal
      open
      stackBody
      title={create ? "Review and create the company" : "Review and save"}
      icon={<SaveGlyph />}
      size="lg"
      dismissable={!busy}
      // ONE WAY OUT PER JOB. Cancel is in the foot; a close control beside
      // the title would be a second, unnamed spelling of it.
      showCloseButton={false}
      onClose={onClose}
      // The secondary road out of the unknown state, at the foot's own start
      // edge. It used to be the first thing in the actions with a flexing
      // span shoved after it, so two spacing mechanisms fought in one band:
      // the end slot's own auto margin and a `flex: 1 1 auto` child inside it.
      footerStart={
        unknown ? (
          <Button variant="secondary" onClick={onCheckAgain} disabled={busy}>
            Check again
          </Button>
        ) : undefined
      }
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!canSave} onClick={() => onSave(summary)}>
            {saveLabel}
          </Button>
        </>
      }
    >
      {/* The engine's answer to the save this reader just asked for, so it is
          announced. The rest of this dialog describes what a save WOULD do and
          is read when the dialog opens. */}
      {phase.kind === "refused" && (
        <Callout variant="danger" role="alert">
          {phase.message}
        </Callout>
      )}
      {phase.kind === "retry" && <Callout variant="warning">{phase.message}</Callout>}
      {phase.kind === "settling" && (
        <Callout variant="warning">Checking whether the save was stored.</Callout>
      )}
      {unknown && (
        <Callout variant="warning">
          Couldn’t confirm the save. {phase.detail} Check again, or save again safely.
        </Callout>
      )}

      {((rules.reason && !busy) || (status === "problems" && documentProblems.length > 0)) && (
        <Callout
          variant={status === "problems" ? "danger" : "warning"}
          title={
            status === "problems" && problemCount > 0
              ? `The engine reported ${plural(problemCount, "problem")}`
              : undefined
          }
        >
          {!busy && rules.reason}
          {status === "problems" && documentProblems.length > 0 && (
            <NoticeList
              label="Problems with the whole configuration"
              notices={documentProblems.map((p, i) => ({
                key: String(i),
                title: engineWords(p.message),
              }))}
            />
          )}
        </Callout>
      )}
      {status === "unchecked" && (
        <Callout variant="warning">Couldn’t check this draft. Saving still validates it.</Callout>
      )}

      {parts.changes.length === 0 ? (
        <section className="col gap-1" aria-label="Changes">
          <Eyebrow as="div">{create ? "The company" : "What changes"}</Eyebrow>
          <p className="muted">No units or seats change.</p>
        </section>
      ) : (
        <ChangeGroups heading={create ? "The company" : "What changes"} groups={parts.changes} />
      )}

      {(follows.length > 0 || acknowledgements.length > 0) && (
        <ConsequenceList heading="What follows" items={[...follows, ...acknowledgements]} />
      )}

      <BuilderWarnings
        draft={warnings}
        title={(n) => `The engine will run this, with ${plural(n, "warning")}`}
      />

      <ConfigField
        label="Audit summary"
        kind="multiline"
        rows={2}
        value={summary}
        onChange={setSummary}
        disabled={busy}
        error={summaryMissing ? "Enter a summary." : undefined}
        help="Kept with this revision, in the audit log and the configuration’s history."
      />
    </Modal>
  );
}
