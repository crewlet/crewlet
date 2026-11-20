/**
 * Remove a model, in as few words as the removal needs: one line saying what
 * uses it, one choice of where all of it moves, and a confirm only where the
 * removal leaves something with no model.
 *
 * THE ENGINE DRAWS THE LIST. A dry run with no repoints is refused with every
 * written reference (`references`, by a seat's handle and job, Ask Crewlet's
 * chat or the Advisor's findings), so the page never works out on its own
 * which fields name the model. The one choice fills a move for every one of
 * them; "Choose per seat" opens a row per place for the rare split. Each set
 * of moves is checked by another dry run as it changes, and the removal and
 * every move land as one revision under the entry's own tag (`If-Match`).
 *
 * NOTHING SAID TWICE, NO PATH (design QA M8). The dialog names what uses the
 * model and asks one question; it never lists the fields the revision writes
 * nor reads the choices back.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  FormField,
  Select,
  type SelectOption,
  Skeleton,
  Text,
  useToast,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { listed, plural } from "~/lib/format.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import { getEntity, type ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { managedSentence, unsettledSentence, type ConfigRefusal } from "~/protocol/configAnswer.ts";
import type { ModelReference } from "~/protocol/models.ts";
import { removeModel, type Removal } from "../api.ts";
import type { ModelsData, ModelView } from "../data.ts";
import { JOB_WORDS, seatName } from "../places.ts";
import { modelLabel, modelMark } from "../modelPicker.tsx";
import { HEALTH } from "../words.ts";

/** What one place becomes: another model, or none (Ask Crewlet turned off, or the Advisor run with no model). */
export type Choice = { to: string } | { to: null };

const UNSET = "__unset__";

/** Whether a reference is Ask Crewlet's model, which pointing at nothing turns off. */
const askCrewlets = (ref: Pick<ModelReference, "at">) => ref.at.assistant === "chat";

/** Whether a reference is the Advisor's model, which pointing at nothing leaves its checks with no model. */
const advisors = (ref: Pick<ModelReference, "at">) => ref.at.advisor === "findings";

/** Whether a reference is a helper's (Ask Crewlet's or the Advisor's), company-wide and no seat's. */
const helpers = (ref: Pick<ModelReference, "at">) => askCrewlets(ref) || advisors(ref);

/** One reference's identity, as the choices are keyed. */
export const refId = (ref: Pick<ModelReference, "at">) =>
  JSON.stringify([
    ref.at.seat ?? "",
    ref.at.job ?? "",
    ref.at.assistant ?? "",
    ref.at.advisor ?? "",
  ]);

/**
 * Whether a place may move to a model: a seat's executor needs a harness that
 * can run one, and Ask Crewlet and the Advisor a model a helper can run on, as
 * the engine says of each (`helper_problem`).
 */
function fits(ref: ModelReference, m: ModelView, removing: string): boolean {
  return (
    m.key !== removing &&
    (ref.at.job !== "executor" || m.executor) &&
    (!helpers(ref) || (m.admin !== null && m.admin.helper_problem === null))
  );
}

/** The models one place may move to. */
export function targetsFor(ref: ModelReference, removing: string, models: readonly ModelView[]) {
  return models.filter((m) => fits(ref, m, removing));
}

/** The models every place may move to: the one choice offers only these. */
export function commonTargets(
  refs: readonly ModelReference[],
  removing: string,
  models: readonly ModelView[],
): ModelView[] {
  return models.filter((m) => m.key !== removing && refs.every((ref) => fits(ref, m, removing)));
}

/**
 * Leaving a place unset drops the model from its field: a field that names
 * another keeps it, and one left empty is unset, so every place offers it.
 */
const LEAVE_UNSET: SelectOption = { value: UNSET, label: "Leave unset" };

/** Models as options: the key, and its health where it is not ready. */
function modelOptions(models: readonly ModelView[]): SelectOption[] {
  return models.map((m) => ({
    value: m.key,
    label: modelLabel(m),
    text: m.key,
    leading: modelMark(m),
    ...(m.entry.health !== "ready" ? { hint: HEALTH[m.entry.health].label } : {}),
  }));
}

/** The choices one place offers in the per-seat list. */
export function optionsFor(
  ref: ModelReference,
  removing: string,
  models: readonly ModelView[],
): SelectOption[] {
  return [...modelOptions(targetsFor(ref, removing, models)), LEAVE_UNSET];
}

/** How sure a model is to answer: ready first, a refused or sitting-out one last. */
const HEALTH_RANK: Record<ModelView["entry"]["health"], number> = {
  ready: 0,
  expiring: 1,
  untested: 2,
  sitting_out: 3,
  rejected: 4,
  no_sign_in: 4,
};

/** The model likeliest to answer, on the removed model's harness where one is as likely. */
function likeliest(
  eligible: readonly ModelView[],
  removing: string,
  models: readonly ModelView[],
): ModelView | undefined {
  const harness = models.find((m) => m.key === removing)?.harness;
  const rank = (m: ModelView) => HEALTH_RANK[m.entry.health] * 2 + (m.harness === harness ? 0 : 1);
  return [...eligible].sort((a, b) => rank(a) - rank(b))[0];
}

/**
 * One place's own first choice, where no model fits every place: its backup
 * where it can take the job, else the eligible model likeliest to answer,
 * else none.
 */
export function defaultChoice(
  ref: ModelReference,
  removing: string,
  models: readonly ModelView[],
): Choice {
  const eligible = targetsFor(ref, removing, models);
  const backup = ref.chain.find((k) => k !== removing && eligible.some((m) => m.key === k));
  if (backup) return { to: backup };
  const best = likeliest(eligible, removing, models);
  return best ? { to: best.key } : { to: null };
}

/**
 * The choices a removal opens with: every place to the one model likeliest to
 * answer that fits them all, or, where none fits them all, each place's own.
 */
export function firstChoices(
  refs: readonly ModelReference[],
  removing: string,
  models: readonly ModelView[],
): Record<string, Choice> {
  const one = likeliest(commonTargets(refs, removing, models), removing, models);
  return Object.fromEntries(
    refs.map((ref) => [refId(ref), one ? { to: one.key } : defaultChoice(ref, removing, models)]),
  );
}

const valueOf = (c: Choice | undefined) => (!c || c.to === null ? UNSET : c.to);
const choiceOf = (value: string): Choice => (value === UNSET ? { to: null } : { to: value });

/** The request every choice makes. */
export function removalOf(
  refs: readonly ModelReference[],
  choices: Record<string, Choice>,
): Removal {
  return {
    repoint: refs.map((ref) => ({ at: ref.at, to: (choices[refId(ref)] ?? { to: null }).to })),
  };
}

/**
 * What a removal does, as the revision's audit summary: "Remove the model
 * sonnet, move 3 uses to default, and turn Ask Crewlet off".
 */
export function summaryOf(key: string, removal: Removal): string {
  const moves = new Map<string, number>();
  let unset = 0;
  for (const r of removal.repoint) {
    if (r.to) moves.set(r.to, (moves.get(r.to) ?? 0) + 1);
    else if (!helpers(r)) unset += 1;
  }
  const sentence = listed([
    `remove the model ${key}`,
    ...[...moves].map(([to, n]) => `move ${plural(n, "use")} to ${to}`),
    ...(unset ? [`leave ${unset} unset`] : []),
    ...(removal.repoint.some((r) => askCrewlets(r) && r.to === null)
      ? ["turn Ask Crewlet off"]
      : []),
    ...(removal.repoint.some((r) => advisors(r) && r.to === null)
      ? ["run the Advisor with no model"]
      : []),
  ]);
  return `${sentence.charAt(0).toUpperCase()}${sentence.slice(1)}`;
}

/** "Used by 9 seats, Ask Crewlet, and the Advisor.": counts and names only for what uses it. */
export function usedByWords(refs: readonly ModelReference[]): string {
  const seats = new Set(refs.filter((r) => !helpers(r)).map((r) => r.at.seat ?? "")).size;
  return `Used by ${listed([
    ...(seats > 0 ? [plural(seats, "seat")] : []),
    ...(refs.some(askCrewlets) ? ["Ask Crewlet"] : []),
    ...(refs.some(advisors) ? ["the Advisor"] : []),
  ])}.`;
}

/**
 * Each place's name in the per-seat list: a seat by its name alone, its job
 * added only where the seat runs on the model for more than one, and Ask
 * Crewlet and the Advisor by theirs.
 */
export function placeNames(
  refs: readonly ModelReference[],
  seats: ModelsData["seats"],
): Map<string, string> {
  const jobs = new Map<string, number>();
  for (const r of refs)
    if (!helpers(r)) jobs.set(r.at.seat ?? "", (jobs.get(r.at.seat ?? "") ?? 0) + 1);
  return new Map(
    refs.map((r) => {
      if (askCrewlets(r)) return [refId(r), "Ask Crewlet"];
      if (advisors(r)) return [refId(r), "The Advisor"];
      const name = seatName(seats, r.at.seat ?? "");
      const several = (jobs.get(r.at.seat ?? "") ?? 0) > 1 && r.at.job;
      return [refId(r), several ? `${name}, ${JOB_WORDS[r.at.job!]}` : name];
    }),
  );
}

/**
 * What the removal leaves with no model, as the one thing the person confirms:
 * Ask Crewlet left unset is off, the Advisor runs with no model, and a seat
 * stops only where no model is left for it to fall back on. A seat left unset
 * while the company has another model runs on the company's fallback, so it
 * is no loss to confirm. Null where nothing is left with no model.
 */
export function leftWithNoModel(
  refs: readonly ModelReference[],
  choices: Record<string, Choice>,
  removing: string,
  models: readonly ModelView[],
): string | null {
  const left = refs.filter((r) => (choices[refId(r)]?.to ?? null) === null);
  const nothingLeft = !models.some((m) => m.key !== removing);
  const seats = nothingLeft
    ? new Set(left.filter((r) => !helpers(r)).map((r) => r.at.seat ?? "")).size
    : 0;
  const parts = [
    ...(left.some(askCrewlets) ? ["turn Ask Crewlet off"] : []),
    ...(left.some(advisors) ? ["run the Advisor with no model"] : []),
    ...(seats > 0 ? [`leave ${plural(seats, "seat")} with no model until one is added`] : []),
  ];
  if (parts.length === 0) return null;
  const sentence = listed(parts);
  return `${sentence.charAt(0).toUpperCase()}${sentence.slice(1)}`;
}

/**
 * A refusal in one sentence a person can act on; an answer that refused
 * nothing says nothing. `stage` is what was asked: a dry run (`check`)
 * removes nothing whatever came back, and only the removal (`remove`) may
 * have landed when its answer did not.
 */
function refusalWords(
  refusal: ConfigWriteOutcome | ConfigRefusal,
  key: string,
  stage: "check" | "remove",
): string {
  switch (refusal.kind) {
    case "valid":
    case "saved":
    case "unchanged":
      return "";
    case "guarded":
      return configGuardedReason(refusal.code);
    case "managed":
      return managedSentence(refusal.managedBy);
    case "conflict":
      return refusal.reason === "no_active_revision"
        ? "No company is configured any more, so there is no model to remove."
        : `The configuration changed while this was open, so nothing was stored. Choose again and remove ${key} again.`;
    case "draining":
      return `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "starting":
      return `This node is still starting and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "unavailable":
      // CERTAIN, like a drain's: the engine refused before writing, and a wait clears it.
      return `Nothing was removed${refusal.detail ? `: ${refusal.detail}` : "."} Try again in a moment.`;
    case "unsettled":
      // A CHECK STORES NOTHING, whoever answered it; only the removal itself may have landed.
      return stage === "check"
        ? `${unsettledSentence(refusal)} Nothing was removed.`
        : `${unsettledSentence(refusal)} ${key} may or may not have been removed. Close this and look at the list before trying again.`;
    case "problems":
      return refusal.problems.map((p) => p.message).join(" ");
  }
}

type Load =
  | { kind: "loading" }
  | { kind: "failed"; message: string }
  | { kind: "ready"; etag: string; refs: ModelReference[] };

type Check = { kind: "checking" } | { kind: "valid" } | { kind: "refused"; message: string };

/** The check of a removal: valid, or the engine's refusal in words. */
export function checkOf(outcome: ConfigWriteOutcome, key: string): Check {
  if (outcome.kind === "valid" || outcome.kind === "saved") return { kind: "valid" };
  return { kind: "refused", message: refusalWords(outcome, key, "check") };
}

export function RemoveDialog({
  model,
  data,
  onClose,
  onDone,
  onRemoved,
}: {
  model: ModelView;
  data: ModelsData;
  onClose: () => void;
  onDone: () => void;
  /** Where the page goes once the model is gone: back to what opened the dialog, where that still exists. */
  onRemoved: () => void;
}) {
  const toast = useToast();
  const lifetime = useLifetimeSignal();
  const key = model.key;
  const [load, setLoad] = useState<Load>({ kind: "loading" });
  const [reads, setReads] = useState(0);
  const [choices, setChoices] = useState<Record<string, Choice>>({});
  /** Whether the per-seat list is open. */
  const [each, setEach] = useState(false);
  /** The consequence the person confirmed, so a different one asks again. */
  const [agreed, setAgreed] = useState<string | null>(null);
  const [check, setCheck] = useState<Check>({ kind: "checking" });
  const [removing, setRemoving] = useState(false);
  /**
   * What the last removal came to, where it did not remove: its title says
   * "Not removed" only where the engine said so. An unsettled answer may have
   * removed it, and a title saying otherwise sent the person to remove it again.
   */
  const [notice, setNotice] = useState<{ readonly title: string; readonly words: string } | null>(
    null,
  );
  const models = data.models;
  const listId = useId();
  const modelsRef = useRef(models);
  modelsRef.current = models;

  // READ THE ENTRY, THEN ASK THE ENGINE WHAT NAMES THE MODEL: a dry run
  // with no repoints answers every reference, or is valid when none does.
  useEffect(() => {
    const abort = new AbortController();
    setLoad({ kind: "loading" });
    void (async () => {
      // THE ENTRY'S OWN TAG is the precondition a removal takes, never the document's.
      const read = await getEntity("llm-providers", key, abort.signal);
      if (read.kind !== "entity") {
        setLoad({
          kind: "failed",
          message:
            read.kind === "missing"
              ? `${key} is no longer in the configuration, so there is nothing to remove.`
              : refusalWords(read, key, "check"),
        });
        return;
      }
      const answer = await removeModel(
        key,
        { repoint: [] },
        { etag: read.etag, summary: "", dryRun: true },
        abort.signal,
      );
      if (answer.outcome.kind !== "valid" && answer.references.length === 0) {
        setLoad({ kind: "failed", message: refusalWords(answer.outcome, key, "check") });
        return;
      }
      const refs = answer.references;
      const current = modelsRef.current;
      setChoices(firstChoices(refs, key, current));
      // NO MODEL FITS THEM ALL: the list opens, since the one choice can only leave them unset.
      setEach(
        refs.length > 1 &&
          commonTargets(refs, key, current).length === 0 &&
          refs.some((ref) => targetsFor(ref, key, current).length > 0),
      );
      setLoad({ kind: "ready", etag: read.etag, refs });
    })().catch(() => {
      // Only this dialog's own abort rejects.
    });
    return () => abort.abort();
  }, [key, reads]);

  const refs = useMemo(() => (load.kind === "ready" ? load.refs : []), [load]);
  const removal = useMemo(() => removalOf(refs, choices), [refs, choices]);
  const removalKey = JSON.stringify(removal);
  const etag = load.kind === "ready" ? load.etag : null;

  // EVERY SET OF MOVES IS CHECKED AS IT CHANGES, so the button below always
  // removes what the engine just accepted.
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
    const summary = summaryOf(key, removal);
    // A REMOVAL THAT HAS STARTED FINISHES: it takes no signal, so closing
    // the dialog or leaving the page never leaves the person unsure whether
    // it happened. What the dialog does with the answer is what its lifetime
    // gates: one that answers after it closed neither toasts nor moves the
    // page the person has gone on to.
    const screen = lifetime();
    const answer = await removeModel(key, removal, { etag, summary, dryRun: false });
    if (screen.aborted) return;
    setRemoving(false);
    if (answer.outcome.kind === "saved") {
      toast.ok(`Removed ${key}.`);
      onDone();
      onRemoved();
      return;
    }
    setNotice({
      title:
        answer.outcome.kind === "unsettled" ? "Not known whether it was removed" : "Not removed",
      words: refusalWords(answer.outcome, key, "remove"),
    });
    if (answer.outcome.kind === "conflict") setReads((n) => n + 1);
  }

  const common = useMemo(() => commonTargets(refs, key, models), [refs, key, models]);
  const names = useMemo(() => placeNames(refs, data.seats), [refs, data.seats]);
  const values = refs.map((ref) => valueOf(choices[refId(ref)]));
  // ONE VALUE WHILE EVERY PLACE SHARES IT; a split made in the list reads as one.
  const shared = values.length > 0 && values.every((v) => v === values[0]) ? values[0] : undefined;
  const consequence = leftWithNoModel(refs, choices, key, models);
  const confirmed = consequence === null || agreed === consequence;
  const ready = load.kind === "ready" && check.kind === "valid";

  return (
    <Modal
      open
      onClose={onClose}
      title={`Remove ${key}`}
      dismissable={!removing}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={removing}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onClick={() => void remove()}
            disabled={!ready || !confirmed}
            loading={removing}
          >
            Remove model
          </Button>
        </>
      }
    >
      {load.kind === "loading" ? (
        <Skeleton variant="text" rows={2} label={`Finding what uses ${key}`} />
      ) : load.kind === "failed" ? (
        <Callout variant="danger" title="The removal could not be prepared">
          {load.message}
        </Callout>
      ) : (
        <div className="models-stack">
          {refs.length === 0 ? (
            <Text as="p" variant="body">
              Nothing uses this model.
            </Text>
          ) : (
            <>
              <Text as="p" variant="body">
                {usedByWords(refs)}
              </Text>
              <FormField
                label={refs.length === 1 ? "Move it to" : "Move them to"}
                labelAction={
                  refs.length > 1 ? (
                    <Button
                      variant="ghost"
                      size="small"
                      aria-expanded={each}
                      aria-controls={listId}
                      onClick={() => setEach((open) => !open)}
                    >
                      {refs.some((r) => !helpers(r)) ? "Choose per seat" : "Choose for each"}
                    </Button>
                  ) : undefined
                }
              >
                {(field) => (
                  <Select
                    id={field.id}
                    value={shared}
                    placeholder="Different for each"
                    options={[...modelOptions(common), LEAVE_UNSET]}
                    disabled={removing}
                    onChange={(next) =>
                      setChoices(
                        Object.fromEntries(refs.map((ref) => [refId(ref), choiceOf(String(next))])),
                      )
                    }
                  />
                )}
              </FormField>
              {each ? (
                <ul id={listId} className="models-repoint" aria-label="Each place">
                  {refs.map((ref) => {
                    const id = refId(ref);
                    const name = names.get(id) ?? "";
                    return (
                      <li key={id} className="models-repoint__row">
                        <span className="models-repoint__name">{name}</span>
                        <Select
                          ariaLabel={`Where ${name} moves`}
                          value={valueOf(choices[id])}
                          options={optionsFor(ref, key, models)}
                          disabled={removing}
                          onChange={(next) =>
                            setChoices((c) => ({ ...c, [id]: choiceOf(String(next)) }))
                          }
                        />
                      </li>
                    );
                  })}
                </ul>
              ) : null}
              {consequence ? (
                <Checkbox
                  tone="danger"
                  label={consequence}
                  checked={agreed === consequence}
                  disabled={removing}
                  onCheckedChange={(on) => setAgreed(on ? consequence : null)}
                />
              ) : null}
            </>
          )}
          {check.kind === "refused" ? (
            <Callout variant="danger" title="This cannot be removed as it stands" live="polite">
              {check.message}
            </Callout>
          ) : null}
          {notice ? (
            <Callout variant="danger" title={notice.title} live="polite">
              {notice.words}
            </Callout>
          ) : null}
        </div>
      )}
    </Modal>
  );
}
