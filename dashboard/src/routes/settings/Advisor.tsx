/**
 * Settings › Advisor: what the company's setup costs it, why, and the change
 * that fixes each (ADR-0115).
 *
 * THE ENGINE NEVER APPLIES A FIX. A change is one entity write the engine
 * built; this screen reads the entity, sets the change's fields, asks the
 * entity route's dry run what the company would become, and shows that answer
 * as the preview. Only Apply sends the write, from this person's session and
 * under the entity's own tag, so a seat somebody edited in between is refused
 * rather than overwritten.
 */

import { useEffect, useMemo, useState } from "react";
import {
  AttentionList,
  Button,
  Callout,
  ChoiceList,
  EmptyState,
  InlineCode,
  Link,
  Modal,
  PageNote,
  Text,
  type AttentionItem,
  type AttentionTone,
} from "@crewlethq/ui";
import { DiffView } from "~/components/DiffView.tsx";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { refusalWords } from "~/lib/ceilings.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { introducedWarnings } from "~/protocol/configAnswer.ts";
import { dryRunEntity, getEntity, putEntity } from "~/protocol/configWrite.ts";
import type { AdvisorFinding, AdvisorOption, AdvisorSeverity } from "~/protocol/advisor.ts";
import type { ConfigWarning } from "~/protocol/types.ts";

/** How often the list is read again at rest: a setup changes by revisions. */
const POLL_MS = 60_000;

const COMPANY = { kind: "company" } as const;

/** Each severity as the list's tone, and the word it is read as. */
const TONE: Record<AdvisorSeverity, AttentionTone> = {
  blocks: "danger",
  costs: "warning",
  note: "info",
};
const OWNER_LABELS = {
  title: "What to change",
  tones: { danger: "Blocks", warning: "Costs", info: "Worth knowing" },
};
const ENGINE_LABELS = {
  title: "What the engine is handling",
  tones: { danger: "Engine", warning: "Engine", info: "Engine" },
};

export function Advisor() {
  const { data, loading, error, refusal, detail, refetch } = useQuery("advisor", undefined, {
    pollMs: POLL_MS,
  });
  const [open, setOpen] = useState<AdvisorFinding | null>(null);
  const owner = data?.findings.filter((f) => f.actor === "owner") ?? [];
  const engine = data?.findings.filter((f) => f.actor === "engine") ?? [];
  return (
    <>
      <PageNote>
        What your company&rsquo;s setup costs it, why, and the change that fixes each. You review
        every change before it is made; the engine never makes one on its own.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data &&
          (owner.length === 0 ? (
            <EmptyState
              size="compact"
              title="Nothing to change"
              description="Your company's setup raises nothing worth changing right now."
            />
          ) : (
            <AttentionList
              items={owner.map((f) => findingItem(f, () => setOpen(f)))}
              labels={OWNER_LABELS}
            />
          ))}
        {engine.length > 0 && (
          <AttentionList items={engine.map((f) => findingItem(f, null))} labels={ENGINE_LABELS} />
        )}
      </QueryState>
      {open && (
        <FixDialog
          finding={open}
          onClose={() => setOpen(null)}
          onApplied={() => {
            setOpen(null);
            refetch();
          }}
        />
      )}
    </>
  );
}

/** One finding as a row of the list: what is wrong, why, and its one action. */
function findingItem(f: AdvisorFinding, review: (() => void) | null): AttentionItem {
  const subject = f.subject.name ?? "";
  let action;
  if (review && f.fix.kind === "config_change" && f.fix.options.length > 0) {
    action = (
      <Button size="small" onClick={review}>
        Review the change
      </Button>
    );
  } else if (review && f.fix.kind === "open_setting") {
    action = (
      <Link href={href(["settings", "config"])} size="caption">
        Open the settings
      </Link>
    );
  }
  return {
    key: `${f.class}\u0000${f.path}\u0000${subject}`,
    tone: TONE[f.severity],
    title: f.title,
    meta: subject || undefined,
    action,
    details: (
      <div className="col gap-1">
        <Text as="p">{f.why}</Text>
        {f.fix.kind === "open_setting" && f.fix.setting && (
          <Text as="p" tone="secondary">
            The setting: <InlineCode>{f.fix.setting}</InlineCode>
          </Text>
        )}
      </div>
    ),
  };
}

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
function FixDialog({
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
    const merged = { ...check.entity, ...option.change.set };
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
  const { entity: kind, id, set } = option.change;
  const read = await getEntity(kind, id, signal);
  if (read.kind !== "entity") {
    return { kind: "refused", message: refusalWords(read, COMPANY).message };
  }
  const merged = { ...read.entity, ...set };
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
    before: fieldLines(set, (field) => read.entity[field]),
    after: fieldLines(set, (field) => set[field]),
    introduced: introducedWarnings(had, after.warnings),
  };
}

/** The fields a change sets, one a line, each read through `value`. */
function fieldLines(set: Record<string, unknown>, value: (field: string) => unknown): string {
  return Object.keys(set)
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
