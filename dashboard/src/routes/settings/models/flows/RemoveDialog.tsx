/**
 * Remove a model: every place that names it is listed first, each moved to
 * another model, left unset, or (for the Dashboard Assistant's chat) the
 * assistant turned off, and the removal and every move land as one revision.
 * Nothing is left pointing at a model that is gone, which the engine would
 * refuse anyway.
 *
 * THE ENGINE DRAWS THE LIST. A dry run with no repoints is refused with every
 * written reference (`references`, by a seat's handle and job or the
 * assistant's chat or deep work), so the page never works out on its own which
 * fields name the model. Each choice is checked by another dry run as it
 * changes (design QA M18), and the real removal states the revision it was
 * built on (`If-Match`).
 *
 * WORDS, NOT PATHS (design QA M8). A place reads "Backend Engineer, coding";
 * the fields the revision writes are listed only under "Every change in the
 * revision".
 */

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  ConfirmField,
  Disclosure,
  Modal,
  Select,
  Skeleton,
  Text,
  useToast,
  type SelectOption,
} from "@crewlethq/ui";
import { useNavigator } from "~/app/router.tsx";
import { listed, plural } from "~/lib/format.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import { getConfig, type ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";
import { managedSentence, type ConfigRefusal } from "~/protocol/configAnswer.ts";
import type { ConfigWarning } from "~/protocol/types.ts";
import type { ModelReference, ModelRepoint } from "~/protocol/models.ts";
import { removeModel, type Removal } from "../api.ts";
import type { ModelsData, ModelView } from "../data.ts";
import { JOB_WORDS, placeWords, seatName } from "../places.ts";
import { HEALTH } from "../words.ts";

/** What one place becomes: another model, unset, or the assistant turned off. */
export type Choice = { to: string } | { to: null } | { off: true };

const UNSET = "__unset__";
const OFF = "__off__";

/** One reference's identity, as the choices are keyed. */
export const refId = (ref: Pick<ModelReference, "at">) =>
  JSON.stringify([ref.at.seat ?? "", ref.at.job ?? "", ref.at.assistant ?? ""]);

/** A written field as a path a person can find in the document: `roles[2].llm_review`. */
function pathOf(segments: readonly (string | number)[]): string {
  return segments
    .map((s, i) => (typeof s === "number" ? `[${s}]` : i === 0 ? s : `.${s}`))
    .join("");
}

/** The models a reference may move to: a coding job needs a CLI that runs one, the assistant a model that denies web tools. */
export function targetsFor(ref: ModelReference, removing: string, models: readonly ModelView[]) {
  return models.filter(
    (m) =>
      m.key !== removing &&
      (ref.at.job !== "executor" || m.executor) &&
      (!ref.at.assistant || m.webDenied),
  );
}

/** Whether a reference may be left unset: the assistant's chat may not while the assistant is on. */
function unsetAllowed(ref: ModelReference, removing: string, assistantOn: boolean): boolean {
  if (ref.chain.some((k) => k !== removing)) return false;
  return !(ref.at.assistant === "chat" && assistantOn);
}

/** The choices a place offers, in the one phrasing every row uses. */
export function optionsFor(
  ref: ModelReference,
  removing: string,
  models: readonly ModelView[],
  assistantOn: boolean,
): SelectOption[] {
  const targets = targetsFor(ref, removing, models);
  return [
    ...targets.map((m) => ({
      value: m.key,
      label: `Move to ${m.key}`,
      ...(m.entry.health !== "ready" ? { hint: HEALTH[m.entry.health].label } : {}),
    })),
    ...(unsetAllowed(ref, removing, assistantOn)
      ? [{ value: UNSET, label: "Leave it unset" }]
      : []),
    ...(ref.at.assistant === "chat" && assistantOn && targets.length === 0
      ? [{ value: OFF, label: "Turn the assistant off" }]
      : []),
  ];
}

/** The first choice a place gets: its own backup where it can take the job, else the first model that can. */
export function defaultChoice(
  ref: ModelReference,
  removing: string,
  models: readonly ModelView[],
  assistantOn: boolean,
): Choice {
  const targets = targetsFor(ref, removing, models).map((m) => m.key);
  const backup = ref.chain.find((k) => k !== removing && targets.includes(k));
  if (backup) return { to: backup };
  if (targets[0]) return { to: targets[0] };
  if (unsetAllowed(ref, removing, assistantOn)) return { to: null };
  return { off: true };
}

const valueOf = (c: Choice) => ("off" in c ? OFF : c.to === null ? UNSET : c.to);
const choiceOf = (value: string): Choice =>
  value === OFF ? { off: true } : value === UNSET ? { to: null } : { to: value };

/** The request every choice makes. */
export function removalOf(
  refs: readonly ModelReference[],
  choices: Record<string, Choice>,
): Removal {
  let assistantOff = false;
  const repoint: ModelRepoint[] = refs.map((ref) => {
    const choice = choices[refId(ref)] ?? { to: null };
    if ("off" in choice) {
      assistantOff = true;
      return { at: ref.at, to: null };
    }
    return { at: ref.at, to: choice.to };
  });
  return { repoint, assistantOff };
}

/**
 * What a removal does, in words: "Removes sonnet, moves 3 uses to default,
 * and leaves 1 unset." for the reader, and the same as an instruction ("Remove
 * sonnet, move ...") for the revision's audit summary.
 */
export function summaryOf(key: string, removal: Removal, mood: "says" | "audit" = "says"): string {
  const verb = (base: string) => (mood === "says" ? `${base}s` : base);
  const moves = new Map<string, number>();
  let unset = 0;
  for (const r of removal.repoint) {
    if (r.to) moves.set(r.to, (moves.get(r.to) ?? 0) + 1);
    else if (!(removal.assistantOff && r.at.assistant === "chat")) unset += 1;
  }
  const parts = [
    `${verb("remove")} the model ${key}`,
    ...[...moves].map(([to, n]) => `${verb("move")} ${plural(n, "use")} to ${to}`),
    ...(unset ? [`${verb("leave")} ${unset} unset`] : []),
    ...(removal.assistantOff ? [`${verb("turn")} the Dashboard Assistant off`] : []),
  ];
  const sentence = listed(parts);
  return `${sentence.charAt(0).toUpperCase()}${sentence.slice(1)}${mood === "says" ? "." : ""}`;
}

/** A field's chain after the removal, as the revision writes it. */
function chainAfter(ref: ModelReference, removing: string, to: string | null): string[] {
  const out: string[] = [];
  for (const k of ref.chain) {
    const next = k === removing ? to : k;
    if (next && !out.includes(next)) out.push(next);
  }
  return out;
}

/** A refusal in one sentence a person can act on; an answer that refused nothing says nothing. */
function refusalWords(refusal: ConfigWriteOutcome | ConfigRefusal, key: string): string {
  switch (refusal.kind) {
    case "valid":
    case "saved":
      return "";
    case "guarded":
      return configGuardedReason(refusal.code);
    case "managed":
      return managedSentence(refusal.managedBy);
    case "conflict":
      return refusal.reason === "no_active_revision"
        ? "No company is configured any more, so there is no model to remove."
        : `The configuration changed while this was open, so nothing was stored. The list is current again: check each choice and remove ${key} again.`;
    case "draining":
      return `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "starting":
      return `This node is still starting and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "unreachable":
      return "The engine did not answer, so the model may or may not have been removed. Close this and look at the list before trying again.";
    case "problems":
      return refusal.problems.map((p) => p.message).join(" ");
  }
}

/** Where a warning lands, in words: a seat's job that ran on the removed model by default moves. */
function warningWords(w: ConfigWarning, data: ModelsData): string {
  if (w.kind === "resolution_moved" && w.seat && w.ref in JOB_WORDS) {
    const job = JOB_WORDS[w.ref as keyof typeof JOB_WORDS];
    return w.to === "nothing"
      ? `${seatName(data.seats, w.seat)}'s ${job} will run on no model.`
      : `${seatName(data.seats, w.seat)}'s ${job} will run on ${w.to}.`;
  }
  return w.message;
}

type Load =
  | { kind: "loading" }
  | { kind: "failed"; message: string }
  | { kind: "ready"; etag: string; refs: ModelReference[] };

type Check =
  | { kind: "checking" }
  | { kind: "valid"; warnings: readonly ConfigWarning[] }
  | { kind: "refused"; message: string };

export function RemoveDialog({
  model,
  data,
  onClose,
  onDone,
}: {
  model: ModelView;
  data: ModelsData;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const nav = useNavigator();
  const lifetime = useLifetimeSignal();
  const key = model.key;
  const assistantOn = data.assistant.enabled;
  const [load, setLoad] = useState<Load>({ kind: "loading" });
  const [reads, setReads] = useState(0);
  const [choices, setChoices] = useState<Record<string, Choice>>({});
  const [check, setCheck] = useState<Check>({ kind: "checking" });
  const [confirm, setConfirm] = useState("");
  const [removing, setRemoving] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const models = data.models;
  const modelsRef = useRef(models);
  modelsRef.current = models;

  // READ THE REVISION, THEN ASK THE ENGINE WHAT NAMES THE MODEL: a dry run
  // with no repoints answers every reference, or is valid when none does.
  useEffect(() => {
    const abort = new AbortController();
    setLoad({ kind: "loading" });
    void (async () => {
      const read = await getConfig(abort.signal);
      if (read.kind !== "document") {
        setLoad({
          kind: "failed",
          message:
            read.kind === "none"
              ? "No company is configured, so there is no model to remove."
              : refusalWords(read, key),
        });
        return;
      }
      const answer = await removeModel(
        key,
        { repoint: [], assistantOff: false },
        { etag: read.etag, summary: "", dryRun: true },
        abort.signal,
      );
      if (answer.outcome.kind !== "valid" && answer.references.length === 0) {
        setLoad({ kind: "failed", message: refusalWords(answer.outcome, key) });
        return;
      }
      const refs = answer.references;
      setChoices(
        Object.fromEntries(
          refs.map((ref) => [refId(ref), defaultChoice(ref, key, modelsRef.current, assistantOn)]),
        ),
      );
      setLoad({ kind: "ready", etag: read.etag, refs });
    })().catch(() => {
      // Only this dialog's own abort rejects.
    });
    return () => abort.abort();
  }, [key, reads, assistantOn]);

  const refs = useMemo(() => (load.kind === "ready" ? load.refs : []), [load]);
  const removal = useMemo(() => removalOf(refs, choices), [refs, choices]);
  const removalKey = JSON.stringify(removal);
  const etag = load.kind === "ready" ? load.etag : null;

  // EVERY CHOICE IS CHECKED AS IT CHANGES, so the button below always removes
  // what the engine just accepted.
  useEffect(() => {
    if (!etag) return;
    const abort = new AbortController();
    setCheck({ kind: "checking" });
    removeModel(key, removal, { etag, summary: "", dryRun: true }, abort.signal).then(
      ({ outcome }) => setCheck(checkOf(outcome, key)),
      () => {
        // Superseded by the next choice, or the dialog closed.
      },
    );
    return () => abort.abort();
    // The removal's content is the dependency, not its identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, etag, removalKey]);

  async function remove() {
    if (!etag) return;
    setRemoving(true);
    setNotice(null);
    const summary = summaryOf(key, removal, "audit");
    // TIED TO THE DIALOG: a removal that answers after it closed neither
    // toasts nor moves the page the person has gone on to.
    const signal = lifetime();
    let answer: Awaited<ReturnType<typeof removeModel>>;
    try {
      answer = await removeModel(key, removal, { etag, summary, dryRun: false }, signal);
    } catch (err) {
      if (isAbort(err)) return;
      throw err;
    }
    if (signal.aborted) return;
    setRemoving(false);
    if (answer.outcome.kind === "saved") {
      toast.ok(`Removed ${key}.`);
      onDone();
      nav.to(["settings", "models"]);
      return;
    }
    setNotice(refusalWords(answer.outcome, key));
    if (answer.outcome.kind === "conflict") setReads((n) => n + 1);
  }

  const ready = load.kind === "ready" && check.kind === "valid";

  return (
    <Modal
      open
      onClose={onClose}
      title={`Remove ${key}`}
      size="lg"
      dismissable={!removing}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={removing}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onClick={() => void remove()}
            disabled={!ready || confirm !== key}
            loading={removing}
          >
            Remove model
          </Button>
        </>
      }
    >
      {load.kind === "loading" ? (
        <Skeleton variant="text" rows={4} label={`Finding what names ${key}`} />
      ) : load.kind === "failed" ? (
        <Callout variant="danger" title="The removal could not be prepared">
          {load.message}
        </Callout>
      ) : (
        <div className="models-stack">
          {refs.length === 0 ? (
            <Text as="p" variant="body">
              Nothing names the model <span className="mono">{key}</span>. Removing it changes no
              seat.
            </Text>
          ) : (
            <>
              <Text as="p" variant="body">
                The model <span className="mono">{key}</span> is named in{" "}
                {plural(refs.length, "place")}. Choose where each one moves; the removal and these
                moves are made together.
              </Text>
              <ul className="models-repoint" aria-label={`Where ${key} is named`}>
                {refs.map((ref) => {
                  const id = refId(ref);
                  const place = placeWords(data.seats, ref.at);
                  const backup = ref.chain.indexOf(key) > 0;
                  const label = backup ? `${place} (backup)` : place;
                  const choice = choices[id] ?? defaultChoice(ref, key, models, assistantOn);
                  return (
                    <li key={id} className="models-repoint__row">
                      <span className="col">
                        <span className="models-repoint__place" title={label}>
                          {label}
                        </span>
                        {ref.at.assistant ? (
                          <Text as="span" variant="caption" tone="tertiary">
                            Only models that deny the CLI's web tools can run the Dashboard
                            Assistant.
                          </Text>
                        ) : null}
                      </span>
                      <Select
                        ariaLabel={`Where ${label} moves`}
                        value={valueOf(choice)}
                        options={optionsFor(ref, key, models, assistantOn)}
                        disabled={removing}
                        onChange={(next) =>
                          setChoices((c) => ({ ...c, [id]: choiceOf(String(next)) }))
                        }
                      />
                    </li>
                  );
                })}
              </ul>
            </>
          )}

          <CheckCallout check={check} summary={summaryOf(key, removal)} data={data} />

          <Disclosure title="Every change in the revision">
            <ul className="models-revision" aria-label="Every change in the revision">
              <li>
                <span className="mono">providers.llm.{key}</span> removed
              </li>
              {refs.map((ref) => {
                const choice = choices[refId(ref)];
                const to = choice && "to" in choice ? choice.to : null;
                const after = chainAfter(ref, key, to);
                return (
                  <li key={refId(ref)}>
                    <span className="mono">{pathOf(ref.segments)}</span>:{" "}
                    <span className="mono">{ref.chain.join(", ")}</span> becomes{" "}
                    {after.length > 0 ? <span className="mono">{after.join(", ")}</span> : "unset"}
                  </li>
                );
              })}
              {removal.assistantOff ? (
                <li>
                  <span className="mono">assistant.enabled</span> becomes{" "}
                  <span className="mono">false</span>
                </li>
              ) : null}
            </ul>
          </Disclosure>

          {notice ? (
            <Callout variant="danger" title="Not removed" live="polite">
              {notice}
            </Callout>
          ) : null}
          <ConfirmField phrase={key} value={confirm} onValueChange={setConfirm} />
        </div>
      )}
    </Modal>
  );
}

function checkOf(outcome: ConfigWriteOutcome, key: string): Check {
  if (outcome.kind === "valid" || outcome.kind === "saved")
    return { kind: "valid", warnings: outcome.warnings };
  return { kind: "refused", message: refusalWords(outcome, key) };
}

function CheckCallout({
  check,
  summary,
  data,
}: {
  check: Check;
  summary: string;
  data: ModelsData;
}) {
  if (check.kind === "checking") {
    return (
      <Text as="p" variant="body" tone="secondary" role="status">
        Checking the change.
      </Text>
    );
  }
  if (check.kind === "refused") {
    return (
      <Callout variant="danger" title="The engine would refuse this" live="polite">
        {check.message}
      </Callout>
    );
  }
  return (
    <Callout variant="info" title="Ready to remove" live="polite">
      <span className="models-stack">
        <span>{summary}</span>
        {check.warnings.length > 0 ? (
          <ul className="models-checklist" aria-label="Also changes">
            {check.warnings.map((w, i) => (
              <li key={i}>{warningWords(w, data)}</li>
            ))}
          </ul>
        ) : null}
      </span>
    </Callout>
  );
}
