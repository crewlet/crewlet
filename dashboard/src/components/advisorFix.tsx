/**
 * A setup change, previewed and sent by a person (ADR-0115): the dialog the
 * advisor opens on a finding, and the one an access request opens on its
 * grant (ADR-0118). The engine builds the change; this reads the entity it
 * writes, asks the entity route's dry run what the company would become, shows
 * that answer, and sends the write only on Apply, under the entity's own tag.
 */

import { useEffect, useMemo, useState } from "react";
import { Button, Callout, ChoiceList, Modal, Text } from "@crewlethq/ui";
import { DiffView } from "~/components/DiffView.tsx";
import { refusalWords } from "~/lib/ceilings.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { introducedWarnings } from "~/protocol/configAnswer.ts";
import { dryRunEntity, getEntity, putEntity } from "~/protocol/configWrite.ts";
import type { AdvisorFinding, AdvisorOption } from "~/protocol/advisor.ts";
import type { ConfigWarning } from "~/protocol/types.ts";

const COMPANY = { kind: "company" } as const;

/** What a check of one option found, or why there is nothing to show. */
type Check =
  | { kind: "checking" }
  | {
      kind: "ready";
      entity: Record<string, unknown>;
      etag: string;
      /** The fields the change sets, one a line, as they are and as they would be. */
      before: string;
      after: string;
      introduced: ConfigWarning[];
    }
  | { kind: "refused"; message: string };

/** The preview of one finding's change, and its Apply. */
export function FixDialog({
  finding,
  onClose,
  onApplied,
}: {
  finding: AdvisorFinding;
  onClose: () => void;
  onApplied: () => void;
}) {
  const access = useConfigWriteAccess();
  const options = finding.fix.options;
  const [chosen, setChosen] = useState(options.length === 1 ? "0" : "");
  const option: AdvisorOption | undefined = chosen === "" ? undefined : options[Number(chosen)];
  const [check, setCheck] = useState<Check | null>(null);
  const [applying, setApplying] = useState(false);
  const [refused, setRefused] = useState("");

  useEffect(() => {
    if (!option) {
      setCheck(null);
      return;
    }
    const controller = new AbortController();
    setCheck({ kind: "checking" });
    setRefused("");
    void preview(option, controller.signal).then(
      (next) => {
        if (!controller.signal.aborted) setCheck(next);
      },
      (err: unknown) => {
        if (!controller.signal.aborted) {
          setCheck({
            kind: "refused",
            message: err instanceof Error ? err.message : "Couldn't reach the engine.",
          });
        }
      },
    );
    return () => controller.abort();
  }, [option]);

  const apply = () => {
    if (!option || check?.kind !== "ready" || applying) return;
    setApplying(true);
    setRefused("");
    const merged = changed(check.entity, option.change);
    void putEntity(
      option.change.entity,
      option.change.id,
      merged,
      check.etag,
      option.change.summary,
      new AbortController().signal,
    ).then(
      (outcome) => {
        setApplying(false);
        if (outcome.kind === "saved") {
          onApplied();
          return;
        }
        setRefused(
          outcome.kind === "valid"
            ? "The engine checked the change but didn't save it. Please try again."
            : refusalWords(outcome, COMPANY).message,
        );
      },
      (err: unknown) => {
        setApplying(false);
        setRefused(err instanceof Error ? err.message : "Couldn't reach the engine.");
      },
    );
  };

  const choices = useMemo(
    () => options.map((o, i) => ({ value: String(i), label: o.label })),
    [options],
  );
  const blocked = !access.can ? access.reason : "";
  return (
    <Modal
      open
      onClose={onClose}
      title={finding.title}
      subtitle={finding.fix.sentence}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={apply}
            loading={applying}
            disabled={check?.kind !== "ready" || blocked !== ""}
          >
            Apply
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        {options.length > 1 && (
          <ChoiceList
            label="Which change"
            options={choices}
            value={chosen === "" ? null : chosen}
            onChange={setChosen}
          />
        )}
        {check?.kind === "checking" && <Text as="p">Checking the change with the engine…</Text>}
        {check?.kind === "ready" && (
          <>
            <DiffView before={check.before} after={check.after} aria-label="The change" />
            {check.introduced.length > 0 && (
              <Callout variant="warning" title="The engine would also say">
                {check.introduced.map((w) => (
                  <Text as="p" key={`${w.path}\u0000${w.message}`}>
                    {w.message}
                  </Text>
                ))}
              </Callout>
            )}
          </>
        )}
        {check?.kind === "refused" && (
          <Callout variant="danger" title="This change can't be made as it is">
            {check.message}
          </Callout>
        )}
        {finding.fix.unchanged && <Text as="p">{finding.fix.unchanged}</Text>}
        {blocked && <Callout variant="info">{blocked}</Callout>}
        {refused && (
          <Callout variant="danger" title="Couldn't save that">
            {refused}
          </Callout>
        )}
      </div>
    </Modal>
  );
}

/**
 * The engine's own reading of an option: the entity as it is now, a dry run of
 * it unchanged and with the change, so the warnings shown are only the ones
 * the change itself would add.
 */
async function preview(option: AdvisorOption, signal: AbortSignal): Promise<Check> {
  const { entity: kind, id } = option.change;
  const read = await getEntity(kind, id, signal);
  if (read.kind !== "entity") {
    return { kind: "refused", message: refusalWords(read, COMPANY).message };
  }
  const merged = changed(read.entity, option.change);
  const fields = [...Object.keys(option.change.merge ?? {}), ...Object.keys(option.change.set)];
  const [before, after] = await Promise.all([
    dryRunEntity(kind, id, read.entity, read.etag, signal),
    dryRunEntity(kind, id, merged, read.etag, signal),
  ]);
  if (after.kind !== "valid") {
    return {
      kind: "refused",
      message:
        after.kind === "saved"
          ? "The engine stored a check it should only have tried."
          : refusalWords(after, COMPANY).message,
    };
  }
  const had = before.kind === "valid" ? before.warnings : [];
  return {
    kind: "ready",
    entity: read.entity,
    etag: read.etag,
    before: fieldLines(fields, (field) => read.entity[field]),
    after: fieldLines(fields, (field) => merged[field]),
    introduced: introducedWarnings(had, after.warnings),
  };
}

/**
 * The entity a change writes: `merge` into the entity as read, so a grant
 * changes its one field and keeps every other, then `set`'s fields replaced.
 */
function changed(
  entity: Record<string, unknown>,
  change: AdvisorOption["change"],
): Record<string, unknown> {
  return { ...mergeInto(entity, change.merge ?? {}), ...change.set };
}

function mergeInto(
  into: Record<string, unknown>,
  from: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...into };
  for (const [key, value] of Object.entries(from)) {
    const had = out[key];
    out[key] = isObject(value) && isObject(had) ? mergeInto(had, value) : value;
  }
  return out;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** The fields a change touches, one a line, each read through `value`. */
function fieldLines(fields: string[], value: (field: string) => unknown): string {
  return [...new Set(fields)]
    .map((field) => `${field}: ${value(field) === undefined ? "not set" : shown(value(field))}`)
    .join("\n");
}

/** An entry of a list (a schedule) by its name and cron, else as written. */
function named(value: unknown): string {
  if (value && typeof value === "object" && "name" in value) {
    const { name, cron } = value as { name: unknown; cron?: unknown };
    return typeof cron === "string" ? `${String(name)} (${cron})` : String(name);
  }
  return JSON.stringify(value);
}

/** A value as a person reads it in the preview. */
function shown(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "boolean") return value ? "on" : "off";
  if (Array.isArray(value)) {
    if (value.length === 0) return "none";
    return value.map((v) => (typeof v === "string" ? v : named(v))).join(", ");
  }
  return named(value);
}

/**
 * THE GRANT AN AGENT ASKED FOR, previewed like any setup change: the engine
 * builds it for the asking seat alone from the catalogue key the option names
 * (`advisor_access`), and nothing is sent until Apply. Only once the grant is
 * saved does the caller send the answer, so a person who chose Allow it never
 * leaves the agent told yes with nothing changed.
 */
export function AccessGrantDialog({
  seat,
  access,
  question,
  onClose,
  onApplied,
}: {
  /** The asking agent's handle. */
  seat: string;
  /** The option's id: the catalogue key. */
  access: string;
  /** The decision's question, the dialog's title. */
  question: string;
  onClose: () => void;
  onApplied: () => void;
}) {
  const { data, error, refusal, detail } = useQuery("advisor_access", { seat, access });
  if (data) {
    const finding: AdvisorFinding = {
      class: "access.requested",
      severity: "note",
      actor: "owner",
      source: "static",
      area: "access",
      subject: { kind: "seat", seat },
      title: question,
      why: "",
      path: "",
      evidence: [],
      fix: data.fix,
    };
    return <FixDialog finding={finding} onClose={onClose} onApplied={onApplied} />;
  }
  return (
    <Modal open onClose={onClose} title={question}>
      {error || refusal ? (
        <Callout variant="danger" title="This access can't be granted as asked">
          {detail || error || "The engine refused to build the change."}
        </Callout>
      ) : (
        <Text as="p">Reading the change from the engine…</Text>
      )}
    </Modal>
  );
}
