/**
 * Review and save: what the save changes, what follows from it, and the
 * write itself.
 *
 * CONSEQUENCES BEFORE THE BUTTON. A rename re-onboards seats, a reorder can
 * change who a seat reports to, a removal clears references and leaves vendor
 * accounts behind. `model/changes.ts` derives all of it from the base and the
 * draft as the engine derived them, and this dialog states each one in a
 * sentence. The consequences that cannot be taken back (a company rename, a
 * handle change, a kind change, a change of tool credentials, removing most
 * of the company) each need an acknowledgement before Save enables.
 *
 * THE WARNINGS ARE THE ENGINE'S, for exactly the body the save will send: the
 * check of the current draft is a dry run of that same request.
 *
 * THE AUDIT SUMMARY CARRIES THE WRITE ID. The operator's sentence is recorded
 * with the revision, followed by the write id, which is what lets the builder
 * recognize this save if its answer is lost (`useSave.ts`).
 */

import { useState } from "react";
import { plural } from "~/lib/format.ts";
import type { ConfigWarning } from "~/protocol/index.ts";
import { ConfigField } from "~/components/ConfigField.tsx";
import { ACKNOWLEDGEMENT_TEXT } from "./dialogParts.tsx";
import type { Acknowledgement, ChangeSet, EntityRef, OnboardingCause } from "./model/changes.ts";
import type { PlacedProblem } from "./model/problems.ts";
import type { CheckStatus, SaveRules } from "./model/scheduler.ts";
import type { BuilderMode } from "./model/transport.ts";
import { signedSummary } from "./model/writes.ts";
import type { SavePhase } from "./useSave.ts";
import { CableGlyph, RefreshGlyph, SaveGlyph } from "@crewlethq/icons/glyphs";
import { Button, Callout, Checkbox, InlineCode, Modal } from "@crewlethq/ui";

/** Why a group of seats onboards again, agreeing with how many there are. */
const ONBOARDING_CAUSE: Record<OnboardingCause, (one: boolean) => string> = {
  company_rename: () => "because the company is renamed",
  seat_rename: (one) => (one ? "because it is renamed" : "because they are renamed"),
  unit_rename: (one) =>
    one ? "because a unit it belongs to is renamed" : "because a unit they belong to is renamed",
  move: (one) => (one ? "because it moves to another unit" : "because they move to another unit"),
};

const names = (refs: readonly EntityRef[]) => refs.map((r) => r.name).join(", ");
const place = (ref: EntityRef) =>
  ref.kind === "company" ? "the top of the organization" : ref.name;
const who = (ref: EntityRef | null) => (ref ? ref.name : "nobody");
const inherited = (flag: boolean) => (flag ? " (inherited)" : "");

/** Every change and consequence, as sentences. */
export function changeSentences(changes: ChangeSet): { changes: string[]; consequences: string[] } {
  const out: string[] = [];
  for (const r of changes.added) out.push(`Adds the ${r.kind} ${r.name}.`);
  for (const r of changes.removed) out.push(`Removes the ${r.kind} ${r.name}.`);
  for (const r of changes.renamed) out.push(`Renames ${r.before} to ${r.after}.`);
  for (const m of changes.moved) {
    out.push(`Moves ${m.ref.name} from ${place(m.from)} to ${place(m.to)}.`);
  }
  for (const e of changes.edited) out.push(`Edits ${e.ref.name}: ${e.fields.join(", ")}.`);
  if (changes.charter.length > 0) out.push(`Edits the charter: ${changes.charter.join(", ")}.`);

  const follow: string[] = [];
  if (changes.companyRename) {
    follow.push(
      `Renames the company from ${changes.companyRename.before} to ${changes.companyRename.after}.`,
    );
  }
  for (const h of changes.handleChanges) {
    follow.push(`${h.ref.name} changes handle from @${h.before} to @${h.after}.`);
  }
  for (const group of changes.onboarding) {
    const one = group.seats.length === 1;
    follow.push(
      `${plural(group.seats.length, "seat")} ${one ? "onboards" : "onboard"} again ${ONBOARDING_CAUSE[group.cause](one)}: ${names(group.seats)}.`,
    );
  }
  for (const u of changes.unitRenames) {
    const schedules =
      u.schedules.length > 0
        ? ` Its schedules get a new identity, so a run due this minute may fire again: ${u.schedules.join(", ")}.`
        : "";
    follow.push(
      `Onboarding pages for ${u.before} are looked up under its new name, ${u.after}.${schedules}`,
    );
  }
  for (const r of changes.reportsTo) {
    follow.push(
      r.orderOnly
        ? `${r.ref.name} reports first to ${who(r.after)} instead of ${who(r.before)}: the same managers, listed in a different order.`
        : `${r.ref.name} reports to ${who(r.after)} instead of ${who(r.before)}.`,
    );
  }
  for (const l of changes.leads) {
    follow.push(
      `${l.ref.name} is led by ${who(l.after)}${inherited(l.afterInherited)} instead of ${who(l.before)}${inherited(l.beforeInherited)}.`,
    );
  }
  for (const c of changes.channels) {
    follow.push(
      `${c.ref.name} uses the channel ${c.after || "none"}${inherited(c.afterInherited)} instead of ${c.before || "none"}${inherited(c.beforeInherited)}.`,
    );
  }
  for (const r of changes.routing) {
    const tool = r.tool === "jira" ? "Jira project" : "Confluence space";
    const shared = r.shared
      ? " More than one owner declares it, and the engine routes to the first it finds."
      : "";
    follow.push(
      `Unrouted work in the ${tool} ${r.scope} goes to ${who(r.after)} instead of ${who(r.before)}.${shared}`,
    );
  }
  for (const s of changes.credentialServers) {
    if (s.gained.length > 0) {
      follow.push(`${s.ref.name} receives tool credentials for ${s.gained.join(", ")}.`);
    }
    if (s.lost.length > 0) {
      follow.push(`${s.ref.name} no longer receives tool credentials for ${s.lost.join(", ")}.`);
    }
  }
  for (const s of changes.strippedFields) {
    const fields = s.fields
      .map((f) => (f.credential ? `${f.name} (a credential, not recoverable)` : f.name))
      .join(", ");
    follow.push(`${s.ref.name} loses fields its new kind cannot hold: ${fields}.`);
  }
  for (const c of changes.clearedReferences) {
    const what =
      c.kind === "gitlab_access_level"
        ? "GitLab access level"
        : c.kind === "unit"
          ? "unit reference"
          : c.kind;
    follow.push(`The ${what} on ${place(c.holder)} naming ${c.from} is cleared.`);
  }
  if (changes.datadogFallback) {
    const { before, after } = changes.datadogFallback;
    follow.push(
      `The Datadog fallback seat changes from ${before ? `@${before}` : "none"} to ${after ? `@${after}` : "none"}.`,
    );
  }
  for (const g of changes.gitlabAccessLevels) {
    follow.push(
      `The GitLab access level for @${g.handle} changes from ${g.before ?? "none"} to ${g.after ?? "none"}.`,
    );
  }
  for (const m of changes.memoryReuse) {
    follow.push(
      `${m.ref.name} takes the handle @${m.handle} of the removed seat ${m.previous}, and its memory reattaches.`,
    );
  }
  if (changes.massRemoval) {
    follow.push(
      `Removes ${changes.massRemoval.removed} of the company's ${changes.massRemoval.total} seats.`,
    );
  }
  return { changes: out, consequences: follow };
}

export function ReviewSaveDialog({
  mode,
  changes,
  rules,
  status,
  warnings,
  problemCount,
  documentProblems,
  needsContact,
  writeId,
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
  warnings: readonly ConfigWarning[];
  problemCount: number;
  documentProblems: readonly PlacedProblem[];
  /** Human seats holding no contact identity, by name. */
  needsContact: readonly string[];
  writeId: string;
  phase: SavePhase;
  onSave: (summary: string) => void;
  onCheckAgain: () => void;
  onClose: () => void;
}) {
  const [summary, setSummary] = useState(changes.summary);
  const [acknowledged, setAcknowledged] = useState<ReadonlySet<Acknowledgement>>(new Set());
  const busy = phase.kind === "saving" || phase.kind === "settling";
  const unknown = phase.kind === "unknown";
  const text = changeSentences(changes);
  const allAcknowledged = changes.acknowledgements.every((a) => acknowledged.has(a));
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
      onClose={onClose}
      footer={
        <>
          {unknown && (
            <Button variant="secondary" onClick={onCheckAgain} disabled={busy}>
              Check again
            </Button>
          )}
          <span className="spacer" />
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
        <Callout variant="warning" icon={<RefreshGlyph />}>
          The engine's answer did not arrive. Checking whether the save was stored.
        </Callout>
      )}
      {unknown && (
        <Callout variant="warning">
          Whether the save was stored could not be confirmed. {phase.detail} Check again, or save
          again: a save that already landed is recognized by its write id rather than stored a
          second time.
        </Callout>
      )}

      {rules.reason && !busy && (
        <Callout variant={status === "problems" ? "danger" : "warning"}>
          {rules.reason}
          {status === "problems" &&
            problemCount > 0 &&
            ` The engine reported ${plural(problemCount, "problem")}.`}
        </Callout>
      )}
      {status === "problems" && documentProblems.length > 0 && (
        <ul className="org-builder-list" aria-label="Problems with the whole configuration">
          {documentProblems.map((p, i) => (
            <li key={i}>{p.message}</li>
          ))}
        </ul>
      )}
      {status === "unreachable" && (
        <Callout variant="warning" icon={<CableGlyph />}>
          The engine could not be reached to check this draft. Saving still validates it.
        </Callout>
      )}

      {needsContact.length > 0 && (
        <section className="col gap-1" aria-label="Seats that need a contact identity">
          <strong>These human seats need a contact identity</strong>
          <ul className="org-builder-list">
            {needsContact.map((name) => (
              <li key={name}>{name}</li>
            ))}
          </ul>
        </section>
      )}

      <section className="col gap-1" aria-label="Changes">
        <strong>{create ? "The company" : "What changes"}</strong>
        {text.changes.length === 0 ? (
          <p className="muted">No units or seats change.</p>
        ) : (
          <ul className="org-builder-list">
            {text.changes.map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        )}
      </section>

      {(text.consequences.length > 0 || !changes.derivedKnown) && (
        <section className="col gap-1" aria-label="Consequences">
          <strong>What follows</strong>
          {!changes.derivedKnown && (
            <p className="muted">
              The engine has not described this draft yet, so consequences that depend on its
              hierarchy (onboarding, reporting lines, leads and routing) are not listed.
            </p>
          )}
          <ul className="org-builder-list">
            {text.consequences.map((line, i) => (
              <li key={i}>{line}</li>
            ))}
          </ul>
        </section>
      )}

      {warnings.length > 0 && (
        <section className="col gap-1" aria-label="Warnings">
          <strong>The engine will run this, with {plural(warnings.length, "warning")}</strong>
          <ul className="org-builder-list">
            {warnings.map((w, i) => (
              <li key={i}>{w.message}</li>
            ))}
          </ul>
        </section>
      )}

      {changes.acknowledgements.map((a) => (
        <Checkbox
          key={a}
          framed
          tone="danger"
          label={ACKNOWLEDGEMENT_TEXT[a]}
          checked={acknowledged.has(a)}
          disabled={busy}
          onCheckedChange={(checked) =>
            setAcknowledged((current) => {
              const next = new Set(current);
              if (checked) next.add(a);
              else next.delete(a);
              return next;
            })
          }
        />
      ))}

      <ConfigField
        label="Audit summary"
        kind="multiline"
        rows={2}
        value={summary}
        onChange={setSummary}
        disabled={busy}
        error={
          summaryMissing
            ? "Enter a summary. The engine records one with every revision."
            : undefined
        }
        help={
          <>
            Recorded with the revision as{" "}
            <InlineCode>{signedSummary(summary, writeId).trim()}</InlineCode>. The write id lets the
            builder recognize this save if its answer is lost.
          </>
        }
      />
    </Modal>
  );
}
