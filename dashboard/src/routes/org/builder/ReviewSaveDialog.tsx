/**
 * Review and save: what the save changes, what follows from it, and the
 * writes themselves.
 *
 * CONSEQUENCES BEFORE THE BUTTON. A unit's rename re-onboards the agents in
 * it, a move changes the tool credentials a seat receives, a removal clears
 * references and leaves vendor accounts behind. `model/changes.ts` reads all
 * of it off the base and the draft, and this dialog states each one in a
 * sentence. The consequences that cannot be taken back (a company rename, a
 * kind change, a change of tool credentials, removing most of the company)
 * each need an acknowledgement before Save enables. What the ENGINE derives
 * from the chart — who reports to whom, what a unit inherits, where unrouted
 * work goes — is not worked out again here, and the dialog says so.
 *
 * THE SAVE IS SHOWN AS IT GOES. It is a sequence of writes (`model/save.ts`):
 * the settings, the chart's structure in batches, each changed object's
 * content. Each is listed with what the engine answered — written, written
 * and still being applied here, refused and why, or not confirmed — because a
 * save that stops part way has landed what came before, and a reader has to
 * see which.
 *
 * THE AUDIT SUMMARY GOES WITH THE SETTINGS. The settings revision records
 * the operator's sentence, followed by the write id, which is what lets the
 * builder recognise that write if its answer is lost. The chart records who
 * made each change on its own, so a save that writes no settings asks for no
 * summary.
 */

import { useState } from "react";
import { plural } from "~/lib/format.ts";
import { ConfigField } from "~/components/ConfigField.tsx";
import { ACKNOWLEDGEMENT_TEXT } from "./dialogParts.tsx";
import type { Acknowledgement, ChangeSet, EntityRef, OnboardingCause } from "./model/changes.ts";
import type { NodeKey } from "./model/keys.ts";
import type { PlacedProblem } from "./model/problems.ts";
import type { StepOutcome } from "./model/save.ts";
import type { CheckStatus, SaveRules } from "./model/scheduler.ts";
import type { BuilderMode } from "./model/transport.ts";
import { signedSummary } from "./model/writes.ts";
import { stepLabel, type SavePhase, type SaveRun } from "./useSave.ts";
import { CableGlyph, RefreshGlyph, SaveGlyph } from "@crewlethq/icons/glyphs";
import { Button, Callout, Checkbox, InlineCode, Modal } from "@crewlethq/ui";

/** Why a group of seats onboards again, agreeing with how many there are. */
const ONBOARDING_CAUSE: Record<OnboardingCause, (one: boolean) => string> = {
  company_rename: () => "because the company is renamed",
  move: (one) => (one ? "because it moves to another unit" : "because they move to another unit"),
};

const names = (refs: readonly EntityRef[]) => refs.map((r) => r.name).join(", ");
const place = (ref: EntityRef) =>
  ref.kind === "company" ? "the top of the organization" : ref.name;

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
  for (const a of changes.addressChanges) {
    const at = a.ref.kind === "seat" ? "@" : "";
    follow.push(
      `${a.ref.name} is addressed as ${at}${a.after} instead of ${at}${a.before}. It keeps its identity${a.ref.kind === "seat" ? ", which its mailbox, diary and schedules are keyed on" : ""}, and ${at}${a.before} goes on reaching it until something else takes that address.`,
    );
  }
  for (const group of changes.onboarding) {
    const one = group.seats.length === 1;
    follow.push(
      `${plural(group.seats.length, "seat")} ${one ? "onboards" : "onboard"} again ${ONBOARDING_CAUSE[group.cause](one)}: ${names(group.seats)}.`,
    );
  }
  for (const u of changes.unitRenames) {
    follow.push(`Onboarding pages for ${u.before} are looked up under its new name, ${u.after}.`);
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
        : c.kind === "datadog_route_to"
          ? "Datadog fallback"
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
  if (changes.massRemoval) {
    follow.push(
      `Removes ${changes.massRemoval.removed} of the company's ${changes.massRemoval.total} seats.`,
    );
  }
  return { changes: out, consequences: follow };
}

/** What a step's answer says, as a phrase beside the step. */
export function outcomeWords(outcome: StepOutcome | undefined, sending: boolean): string {
  if (!outcome) return sending ? "Writing…" : "Not sent";
  switch (outcome.kind) {
    case "applied":
      return outcome.revisionId
        ? `Written as revision ${outcome.revisionId.slice(0, 10)}`
        : outcome.position
          ? `Written at ${outcome.position}`
          : "Written";
    case "pending":
      return `Written at ${outcome.position}; this node is still applying it`;
    case "unknown":
      return `Not confirmed (operation ${outcome.opId})`;
    case "refused":
      return `Refused: ${outcome.detail}`;
    case "conflict":
      return "Somebody else's write got there first";
  }
}

export function ReviewSaveDialog({
  mode,
  changes,
  rules,
  status,
  warnings,
  problemCount,
  documentProblems,
  withoutContact,
  writesSettings,
  runtimeVisible,
  writeId,
  phase,
  run,
  nameOf,
  onSave,
  onRetry,
  onClose,
}: {
  mode: BuilderMode;
  changes: ChangeSet;
  rules: SaveRules;
  status: CheckStatus;
  /** The current check's warnings: the draft's own, and the settings' dry run. */
  warnings: readonly PlacedProblem[];
  problemCount: number;
  documentProblems: readonly PlacedProblem[];
  /**
   * Human seats holding no contact identity, by name. A notice rather than a
   * refusal: the engine admits such a seat, and the chart check reports it.
   */
  withoutContact: readonly string[];
  /** Whether the save writes the settings, which is what records the audit summary. */
  writesSettings: boolean;
  /** Whether the chart showed this reader the runtime half. */
  runtimeVisible: boolean;
  writeId: string;
  phase: SavePhase;
  run: SaveRun | null;
  nameOf: (key: NodeKey) => string;
  onSave: (summary: string) => void;
  onRetry: () => void;
  onClose: () => void;
}) {
  const [summary, setSummary] = useState(changes.summary);
  const [acknowledged, setAcknowledged] = useState<ReadonlySet<Acknowledgement>>(new Set());
  const busy = phase.kind === "confirming" || phase.kind === "saving" || phase.kind === "settling";
  const stopped = phase.kind === "stopped" ? phase : null;
  const unknown = stopped?.unknown === true;
  const text = changeSentences(changes);
  const allAcknowledged = changes.acknowledgements.every((a) => acknowledged.has(a));
  const summaryMissing = writesSettings && summary.trim() === "";
  const canSave = rules.save && allAcknowledged && !summaryMissing && !busy && !unknown;
  const create = mode === "create";
  const chartChanges =
    changes.added.length +
      changes.removed.length +
      changes.moved.length +
      changes.renamed.length +
      changes.edited.length >
    0;

  const saveLabel = busy
    ? phase.kind === "confirming"
      ? "Checking the chart"
      : phase.kind === "settling"
        ? "Checking the settings"
        : "Saving"
    : rules.waiting
      ? "Waiting for the check"
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
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            {unknown ? "Close" : "Cancel"}
          </Button>
          {unknown ? (
            <Button variant="primary" onClick={onRetry}>
              Retry
            </Button>
          ) : (
            <Button variant="primary" disabled={!canSave} onClick={() => onSave(summary)}>
              {saveLabel}
            </Button>
          )}
        </>
      }
    >
      {/* The engine's answer to the save this reader just asked for, so it is
          announced. The rest of this dialog describes what a save WOULD do and
          is read when the dialog opens. */}
      {stopped && (
        <Callout variant={unknown ? "warning" : "danger"} role="alert">
          {stopped.message}
          {!unknown &&
          run &&
          run.outcomes.some((o) => o?.kind === "applied" || o?.kind === "pending")
            ? " What was written before it stays written; the draft now holds only what is left to save."
            : ""}
        </Callout>
      )}
      {phase.kind === "settling" && (
        <Callout variant="warning" icon={<RefreshGlyph />}>
          The engine's answer to the settings did not arrive. Checking whether they were stored.
        </Callout>
      )}

      {run && (
        <section className="col gap-1" aria-label="The save">
          <strong>The save</strong>
          <ol className="org-builder-list">
            {run.steps.map((step, i) => (
              <li key={step.id}>
                {stepLabel(step, nameOf).replace(/^./, (c) => c.toUpperCase())}:{" "}
                {outcomeWords(run.outcomes[i], phase.kind === "saving" && phase.at === i)}
              </li>
            ))}
          </ol>
        </section>
      )}

      {rules.reason && !busy && !stopped && (
        <Callout variant={status === "problems" ? "danger" : "warning"}>
          {rules.reason}
          {status === "problems" &&
            problemCount > 0 &&
            ` The check found ${plural(problemCount, "problem")}.`}
        </Callout>
      )}
      {status === "problems" && documentProblems.length > 0 && (
        <ul className="org-builder-list" aria-label="Problems with the whole company">
          {documentProblems.map((p, i) => (
            <li key={i}>{p.message}</li>
          ))}
        </ul>
      )}
      {status === "unreachable" && (
        <Callout variant="warning" icon={<CableGlyph />}>
          The engine could not be reached to check this draft. Saving still reads the chart first,
          and every write is decided where it lands.
        </Callout>
      )}
      {!runtimeVisible && (
        <Callout variant="neutral">
          The engine does not show this reader the runtime half of the chart — model chains, tool
          credentials, contact identities — so this save leaves it as it is on every seat and unit.
        </Callout>
      )}

      {withoutContact.length > 0 && (
        <section className="col gap-1" aria-label="Human seats with no contact identity">
          <strong>These human seats have no contact identity</strong>
          <p className="t-caption">
            No agent can @-mention them, so agents hand them work in the tracker instead. That is
            right for a person who works only through the dashboard; the chart check keeps naming
            each one until an identity is added in the seat's editor.
          </p>
          <ul className="org-builder-list">
            {withoutContact.map((name) => (
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

      {(text.consequences.length > 0 || chartChanges) && (
        <section className="col gap-1" aria-label="Consequences">
          <strong>What follows</strong>
          {chartChanges && (
            <p className="muted">
              Who reports to whom, the lead and channel a unit inherits and where unrouted work goes
              are derived by the engine from the chart once it is saved, so they are not listed
              here.
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

      {writesSettings && (
        <ConfigField
          label="Audit summary"
          kind="multiline"
          rows={2}
          value={summary}
          onChange={setSummary}
          disabled={busy}
          error={
            summaryMissing
              ? "Enter a summary. The engine records one with every settings revision."
              : undefined
          }
          help={
            <>
              Recorded with the settings revision as{" "}
              <InlineCode>{signedSummary(summary, writeId).trim()}</InlineCode>. The write id lets
              the builder recognize the settings write if its answer is lost; the org chart records
              who made each of its changes on its own.
            </>
          }
        />
      )}
    </Modal>
  );
}
