/**
 * Add a model, or edit one, step by step: its name and harness, how it signs in,
 * the model, where it runs a seat's executor, what it is used for, its limits and a
 * test call. Review checks the whole change with the engine before anything
 * is stored, and the engine's problems land on the fields they are about,
 * each with the step that fixes it.
 *
 * EDITING IS THE SAME WIZARD, prefilled from the model (`editing`): every step
 * is reachable at once, the name and the harness are its identity and stay, a
 * stored credential is shown as stored and replaced only on request, and
 * Review is a footer button away from any step (design QA Minor 13).
 *
 * THE COMMIT ORDER IS SETUP'S, value then pointer (design §5.1): a pasted
 * credential is tested in a fresh home and stored first, with the draft entry
 * it is for, and the document that names it is written after, as one
 * revision against the revision Review checked. A refused document leaves the
 * stored secret inert and the draft open, so the save can be tried again
 * without storing the value twice.
 *
 * NOTHING IS SAID WRONG BEFORE THE READER ACTS (design QA M5). A step's own
 * problems show once Continue is pressed on it, or once Review is reached;
 * a deep link into a step opens it quiet, with its field focused.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Button, ConfirmModal, SetupWizard, useToast, type StepperStep } from "@crewlethq/ui";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { useNavigator } from "~/app/router.tsx";
import { listed } from "~/lib/format.ts";
import { intentOf, usesForIntent, withCategory, noUses } from "~/lib/modelUses.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import {
  introducedWarnings,
  managedSentence,
  unsettledSentence,
  type ConfigRefusal,
} from "~/protocol/configAnswer.ts";
import { getConfig } from "~/protocol/configWrite.ts";
import { isAbort } from "~/protocol/rest.ts";
import type { CompanyDocument, ConfigProblem } from "~/protocol/types.ts";
import type { CheckLine } from "~/protocol/models.ts";
import {
  checkModel,
  storeCredential,
  testCredential,
  writeDocument,
  type ModelAnswer,
} from "../api.ts";
import type { ModelsData, ModelView } from "../data.ts";
import { failureWords, type SignInFacts } from "../flows/TestDialog.tsx";
import type { WizardAdd, WizardAt, WizardStep } from "../parts.tsx";
import { CATEGORY_WORDS } from "../words.ts";
import {
  METHODS,
  STEP_LABELS,
  baseOf,
  canExecute,
  credentialValues,
  distinctProblems,
  draftOf,
  draftProblems,
  emptyDraft,
  entityOf,
  messageOf,
  nextDocument,
  pastesCredential,
  stepOf,
  stepsFor,
  storedCloudSecrets,
  suggestedVariable,
  type Harness,
  type Draft,
  type Kept,
  type NextDocument,
} from "./draft.ts";
import { MODEL_DRAFT_HELP, modelDraftFields } from "./assistantDraft.ts";
import { findOpener, firstInvalid, focusWhenShown, openerNow, pageHeading } from "./focus.ts";
import { HarnessStep } from "./steps/HarnessStep.tsx";
import { LimitsStep } from "./steps/LimitsStep.tsx";
import { ModelStep } from "./steps/ModelStep.tsx";
import { Review, type ReviewCheck } from "./steps/Review.tsx";
import { SignInStep, sharedSecret } from "./steps/SignInStep.tsx";
import { TestStep, routeOf, testWords, type TestState } from "./steps/TestStep.tsx";
import { UseItFor } from "./steps/UseItFor.tsx";
import { WhereItRuns } from "./steps/WhereItRuns.tsx";

const LEADS: Record<WizardStep, string> = {
  harness:
    "Name the model and choose the harness it runs. Seats and Ask Crewlet name it, so keep it short.",
  signin: "How this model proves who it is to the vendor.",
  model: "The model the harness runs, in its own naming.",
  runs: "Where a seat's executor runs when it launches on this model.",
  assign: "What should this model do, and for which seats?",
  limits:
    "How many harness processes run at once, how long one may go without progress, and its web tools.",
  test: "Run the harness once with these settings before anything is stored. It costs a few tokens.",
  review: "Everything this saves, as one revision. The engine checks it before you save it.",
};

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

/**
 * A refusal of the configuration surface in one sentence a person can act on,
 * at the `check` (the read and the dry runs, which store nothing whoever
 * answered) or the `save`, the one write that may have landed.
 */
function refusalWords(refusal: ConfigRefusal, stage: "check" | "save"): string {
  switch (refusal.kind) {
    case "guarded":
      return configGuardedReason(refusal.code);
    case "managed":
      return managedSentence(refusal.managedBy);
    case "conflict":
      return refusal.reason === "no_active_revision"
        ? "No company is configured any more, so there is nothing to add a model to."
        : "The configuration changed since this was checked, so nothing was stored. Check the change again.";
    case "draining":
      return `This node is draining and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "starting":
      return `This node is still starting and stored nothing${refusal.detail ? `: ${refusal.detail}` : "."}`;
    case "unavailable":
      // CERTAIN, like a drain's: the engine refused before writing, and a wait clears it.
      return `Nothing was stored${refusal.detail ? `: ${refusal.detail}` : "."} Try again in a moment.`;
    case "unsettled":
      return stage === "check"
        ? `${unsettledSentence(refusal)} Nothing was stored. Check the change again.`
        : `${unsettledSentence(refusal)} The change may or may not have been stored. Reopen the model before trying again.`;
    case "problems":
      return "The engine refused the change. Correct what is marked and try again.";
  }
}

/** The problems a model route's refusal carried, in the engine's shape. */
function problemsIn(body: Record<string, unknown>): ConfigProblem[] {
  return Array.isArray(body.problems)
    ? (body.problems.filter((p) => isRecord(p) && typeof p.message === "string") as ConfigProblem[])
    : [];
}

/**
 * A credential route's refusal in words, placed at the field it is about:
 * the value the vendor refused, the vendor that did not answer, the secret
 * name already stored, or the entry the engine refused.
 */
function credentialRefusal(
  answer: Extract<ModelAnswer<unknown>, { kind: "refused" }>,
  names: readonly string[],
): { message: string; problems: ConfigProblem[] } {
  const at = (segments: (string | number)[], message: string): ConfigProblem => ({
    path: segments.join("."),
    segments,
    kind: "invalid",
    message,
  });
  switch (answer.code) {
    case "credential_rejected": {
      const problems = problemsIn(answer.body);
      return {
        message: answer.sentence,
        problems:
          problems.length > 0 ? problems : names.map((n) => at(["values", n], answer.sentence)),
      };
    }
    case "credential_untested":
      return {
        message: answer.sentence,
        problems: names.slice(0, 1).map((n) => at(["values", n], answer.sentence)),
      };
    case "already_exists": {
      const held = Array.isArray(answer.body.names)
        ? answer.body.names.filter((n): n is string => typeof n === "string")
        : [...names];
      const message = `${listed(held)} ${held.length === 1 ? "is" : "are"} already stored, and another model may read ${held.length === 1 ? "it" : "them"}. Choose another name.`;
      return { message, problems: [at(["variable"], message)] };
    }
    default:
      return { message: answer.sentence, problems: problemsIn(answer.body) };
  }
}

/**
 * The test's verdict from the doctor's lines: what it said when it answered,
 * and when it did not, the cause in the words of the draft's credential.
 */
function checkVerdict(lines: readonly CheckLine[], signIn: SignInFacts) {
  const failed = lines.find((l) => !l.ok);
  if (failed)
    return {
      ok: false,
      message: `${failureWords(failed.kind, signIn, failed.meaning)}.${failed.remedy ? ` ${failed.remedy}` : ""}`,
    };
  const smoke = lines.find((l) => l.check === "smoke");
  return { ok: true, message: smoke?.detail ?? "It answers." };
}

/** The draft the wizard opens with: a new model or the model as written, plus the uses its action adds. */
function openingDraft(
  data: ModelsData,
  editing: ModelView | undefined,
  target: WizardAt | undefined,
) {
  return withAdded(
    editing ? draftOf(editing) : newDraft(data),
    target?.add ?? [],
    new Set(data.seats.map((s) => s.handle)),
  );
}

/** A new model's draft: a variable no other model reads, and for the company's first model, everything. */
function newDraft(data: ModelsData): Draft {
  const d = emptyDraft();
  const taken = data.models.map((m) => m.admin?.sign_in.variable ?? "").filter(Boolean);
  d.variable = suggestedVariable(d, taken);
  // THE COMPANY'S FIRST MODEL does everything for every seat until a second one arrives.
  if (data.models.length === 0 && data.seats.length > 0) {
    d.intent = "all";
    d.uses = usesForIntent(
      "all",
      data.seats.map((s) => s.handle),
      false,
    );
  }
  return d;
}

/**
 * The draft with the uses an action asked for added (a seat's job picked on
 * By job, the seats moved off a limit, the assistant), for Use it for to show
 * checked and Review to save.
 *
 * THE ADDRESS ONLY PRESELECTS, and only what the company has now: a seat it
 * no longer has (a link kept from before a seat was removed, or typed by
 * hand) is dropped without a word, and so is coding on a harness that runs none.
 * The person still sees every use on Use it for, and Review before a write.
 */
function withAdded(d: Draft, add: readonly WizardAdd[], seats: ReadonlySet<string>): Draft {
  let uses = d.uses;
  for (const a of add) {
    if (!("seat" in a)) uses = { ...uses, assistant: true };
    else if (seats.has(a.seat) && (a.job !== "coding" || canExecute(d)))
      uses = withCategory(uses, a.seat, a.job, true);
  }
  return uses === d.uses ? d : { ...d, uses, intent: intentOf(uses) };
}

export function ModelWizard({
  data,
  editing,
  target,
  onClose,
  onDevice,
}: {
  data: ModelsData;
  /** The model to edit; left out, the wizard adds one. */
  editing?: ModelView;
  /** Where an edit opens: the step, the field to focus, a seat's job to add. */
  target?: WizardAt;
  onClose: () => void;
  onDevice: (key: string) => void;
}) {
  const toast = useToast();
  const nav = useNavigator();
  // THE CONTROL THAT OPENED THE WIZARD is read while its page is still on screen.
  const [opener] = useState(openerNow);
  const [original] = useState(() => (editing ? draftOf(editing) : null));
  const [base] = useState(() => (editing ? baseOf(editing) : undefined));
  const [initial] = useState(() => openingDraft(data, editing, target));
  const [draft, setDraft] = useState<Draft>(initial);
  const steps = stepsFor(draft);
  const [step, setStep] = useState<WizardStep>(() =>
    target && stepsFor(initial).includes(target.step) ? target.step : "harness",
  );
  // WHAT IS BEING SET UP reaches the Dashboard Assistant, read-only and with no credential (assistantDraft.ts).
  const draftFields = useMemo(
    () => modelDraftFields(data.seats.map((s) => s.handle)),
    [data.seats],
  );
  useAssistantDraft<Draft>({
    kind: "client",
    id: editing?.key ?? null,
    step,
    help: MODEL_DRAFT_HELP,
    scope: editing
      ? `The changes to the model ${editing.key} (not saved)`
      : "The model you are adding (not saved)",
    value: draft,
    fields: draftFields,
  });
  const [reached, setReached] = useState<WizardStep>(editing ? "review" : step);
  const [attempted, setAttempted] = useState<ReadonlySet<WizardStep>>(new Set());
  // A RENEW OR A REPLACE opens the stored secret for a new value at once.
  const [replacing, setReplacing] = useState(
    !!original && target?.focus === "credential" && pastesCredential(original),
  );
  const [variableTouched, setVariableTouched] = useState(!!editing);
  const [test, setTest] = useState<TestState>({ kind: "idle" });
  const [engineProblems, setEngineProblems] = useState<ConfigProblem[]>([]);
  const [check, setCheck] = useState<ReviewCheck>({ kind: "idle" });
  const [prepared, setPrepared] = useState<{
    etag: string;
    before: CompanyDocument;
    next: NextDocument;
  } | null>(null);
  const [saving, setSaving] = useState(false);
  /** The draft whose credential is stored already, so a retried save does not store it twice. */
  const [storedFor, setStoredFor] = useState<string | null>(null);
  const [discarding, setDiscarding] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const inflight = useRef<AbortController | null>(null);
  const lifetime = useLifetimeSignal();

  const editingArg = original && base ? { base, was: original.uses } : undefined;
  const existing = data.models.map((m) => m.key).filter((k) => k !== editing?.key);
  const taken = useMemo(
    () =>
      data.models
        .filter((m) => m.key !== editing?.key)
        .map((m) => m.admin?.sign_in.variable ?? "")
        .filter(Boolean),
    [data.models, editing?.key],
  );
  const kept: Kept = {
    credential:
      !!original &&
      !replacing &&
      pastesCredential(draft) &&
      draft.method === original.method &&
      draft.variable === original.variable &&
      !!original.variable,
    cloud:
      draft.method === "cloud" && original?.method === "cloud" && draft.cloud === original.cloud
        ? storedCloudSecrets(base)
        : {},
  };
  const values = credentialValues(draft);
  const names = Object.keys(values);
  const entity = entityOf(draft, base);
  const signature = JSON.stringify({ entity, values });
  // AN ENTRY UNCHANGED FROM THE ACTIVE ONE is not sent as a draft: a rotation
  // tests the model as it runs, and is allowed on a document another system
  // manages, where a draft entry would be a change it refuses.
  const entityChanged = !base || JSON.stringify(entity) !== JSON.stringify(base.raw);
  const storedNames = new Set([original?.variable ?? "", ...Object.values(kept.cloud)]);
  const createOnly = names.every((n) => !storedNames.has(n));

  // HOW THE DRAFT SIGNS IN, as a test's failure is named: a key is a key, a token a token.
  const signIn: SignInFacts = {
    route: routeOf(draft.method, editing?.entry.sign_in.route ?? "none"),
    expiresAt: null,
  };
  const local = draftProblems(draft, existing, kept);
  const shown = [...local.filter((p) => attempted.has(stepOf(p, draft.key))), ...engineProblems];
  const listedProblems = distinctProblems(local, engineProblems, draft.key);
  const errorWhere = (match: (s: (string | number)[]) => boolean) => {
    const p = shown.find((x) => match(x.segments ?? []));
    return p ? messageOf(p) : undefined;
  };
  const field = (name: string) => (s: (string | number)[]) =>
    s[0] === "providers" && s[1] === "llm" && s[2] === draft.key && s[3] === name;

  const set = useCallback(
    (patch: Partial<Draft>) => {
      setDraft((d) => {
        const next = { ...d, ...patch };
        // THE SECRET'S NAME FOLLOWS the name and the method until the reader names one;
        // an edit on its own method keeps the secret it reads.
        const own = original && next.method === original.method && original.variable;
        if (!("variable" in patch) && !variableTouched && pastesCredential(next) && !own)
          next.variable = suggestedVariable(next, taken);
        return next;
      });
      setEngineProblems([]);
      setCheck({ kind: "idle" });
      setPrepared(null);
    },
    [original, variableTouched, taken],
  );

  const changeHarness = (harness: Harness) => {
    const executor = canExecute({ harness });
    const dropsCoding = !executor && (draft.intent === "all" || draft.intent === "coding");
    set({
      harness,
      method: METHODS[harness][0]!,
      credential: "",
      model: "",
      ...(executor ? {} : { runIn: "" }),
      ...(dropsCoding
        ? { intent: "none", uses: { ...noUses(), assistant: draft.uses.assistant } }
        : {}),
    });
  };

  // FOCUS ON OPEN (design QA M6): the field the action is about, else the step's heading.
  useEffect(() => {
    focusWhenShown(() => {
      const area = target?.focus
        ? root.current?.querySelector<HTMLElement>(`[data-focus="${target.focus}"]`)
        : null;
      return (
        area?.querySelector<HTMLElement>("input, textarea") ??
        area ??
        root.current?.querySelector<HTMLElement>(".crewlet-setup-wizard__heading") ??
        null
      );
    });
    return () => inflight.current?.abort();
    // Once, as the wizard opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /** The test call: the credential route's dry run for a pasted credential, the model's check otherwise. */
  async function runTest(signal?: AbortSignal): Promise<boolean> {
    const sig = signature;
    setTest({ kind: "running", for: sig });
    const draftEntity = entityChanged ? { entity } : {};
    if (names.length > 0) {
      const answer = await testCredential(
        draft.key,
        { values, ...draftEntity, createOnly },
        signal,
      );
      if (answer.kind === "ok") {
        setTest({
          kind: "passed",
          for: sig,
          verified: answer.value.verified,
          check: answer.value.check,
          limited: answer.value.limited,
        });
        return true;
      }
      const refusal = credentialRefusal(answer, names);
      const doctor = isRecord(answer.body.check) ? (answer.body.check as never) : null;
      setTest({ kind: "failed", for: sig, message: refusal.message, check: doctor });
      setEngineProblems(refusal.problems);
      return false;
    }
    const answer = await checkModel(draft.key, { smoke: true, ...draftEntity }, signal);
    if (answer.kind === "ok") {
      const verdict = checkVerdict(answer.value.lines, signIn);
      setTest(
        verdict.ok
          ? {
              kind: "passed",
              for: sig,
              verified: verdict.message,
              check: answer.value,
              limited: null,
            }
          : { kind: "failed", for: sig, message: verdict.message, check: answer.value },
      );
      return verdict.ok;
    }
    setTest({ kind: "failed", for: sig, message: answer.sentence, check: null });
    setEngineProblems(problemsIn(answer.body));
    return false;
  }

  /** Review's check: the credential's test where one is pasted, then the whole next document's dry run. */
  async function runCheck() {
    inflight.current?.abort();
    const ctrl = new AbortController();
    inflight.current = ctrl;
    try {
      if (local.length > 0) {
        setCheck({ kind: "problems" });
        return;
      }
      if (
        names.length > 0 &&
        storedFor !== signature &&
        !(test.kind === "passed" && test.for === signature)
      ) {
        setCheck({ kind: "checking", what: "credential" });
        if (!(await runTest(ctrl.signal))) {
          setCheck({ kind: "problems" });
          return;
        }
      }
      setCheck({ kind: "checking", what: "document" });
      const read = await getConfig(ctrl.signal);
      if (read.kind !== "document") {
        setCheck({
          kind: "blocked",
          message:
            read.kind === "none"
              ? "No company is configured yet. Set one up first."
              : refusalWords(read, "check"),
        });
        return;
      }
      const next = nextDocument(read.document, draft, editingArg);
      const [outcome, baseline] = await Promise.all([
        writeDocument(next.doc, { etag: read.etag, summary: "", dryRun: true }, ctrl.signal),
        writeDocument(read.document, { etag: read.etag, summary: "", dryRun: true }, ctrl.signal),
      ]);
      if (outcome.kind === "valid") {
        setPrepared({ etag: read.etag, before: read.document, next });
        setCheck({
          kind: "ready",
          warnings: introducedWarnings(
            baseline.kind === "valid" ? baseline.warnings : [],
            outcome.warnings,
          ),
        });
      } else if (outcome.kind === "problems") {
        setEngineProblems([...outcome.problems]);
        setCheck({ kind: "problems" });
      } else if (outcome.kind !== "saved") {
        setCheck({ kind: "blocked", message: refusalWords(outcome, "check") });
      }
    } catch (err) {
      if (!isAbort(err)) throw err;
    }
  }

  // REVIEW IS CHECKED whenever it is opened; a change to the draft clears the answer.
  useEffect(() => {
    if (step === "review" && check.kind === "idle") void runCheck();
    // `runCheck` reads the draft as it stands when Review opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, check.kind]);

  /** One line for the revision's audit: what was added or edited, and what for. */
  function summary(): string {
    const verb = original ? `Edit model ${draft.key}` : `Add model ${draft.key}`;
    const seats = new Set(Object.values(draft.uses.jobs).flat()).size;
    const what =
      draft.intent === "all"
        ? "every job"
        : draft.intent === "none" || draft.intent === "custom"
          ? ""
          : draft.intent === "assistant"
            ? "Ask Crewlet"
            : CATEGORY_WORDS[draft.intent].label.toLowerCase();
    if (!what) return verb;
    return `${verb} for ${what}${seats > 0 ? ` (${seats} ${seats === 1 ? "seat" : "seats"})` : ""}`;
  }

  async function save() {
    if (!prepared || check.kind !== "ready") return;
    // THE SAVE RUNS TO ITS END ONCE STARTED, whatever happens to the wizard:
    // its requests take no signal, since stopped between the credential and
    // the document it would leave a sealed secret no document names. What
    // the wizard does with each answer (its state, the toast, the move to the
    // model's page) is what its lifetime gates: a save that answers after the
    // person left acts for nothing on screen.
    const screen = lifetime();
    setSaving(true);
    try {
      let { etag, next } = prepared;
      if (names.length > 0 && storedFor !== signature) {
        const draftEntity = entityChanged ? { entity } : {};
        const stored = await storeCredential(
          draft.key,
          { values, ...draftEntity, createOnly },
          etag,
        );
        if (stored.kind === "refused") {
          if (screen.aborted) return;
          const refusal = credentialRefusal(stored, names);
          setEngineProblems(refusal.problems.length > 0 ? refusal.problems : []);
          setCheck(
            refusal.problems.length > 0
              ? { kind: "problems" }
              : { kind: "blocked", message: refusal.message },
          );
          return;
        }
        if (!screen.aborted) setStoredFor(signature);
        // A ROTATION RE-ACTIVATES THE REVISION that reads it, which moves the
        // tag: the document is read again, and written on only if nothing
        // but that re-activation happened in between.
        if (stored.value.reload) {
          const read = await getConfig();
          if (
            read.kind !== "document" ||
            JSON.stringify(read.document) !== JSON.stringify(prepared.before)
          ) {
            if (screen.aborted) return;
            setPrepared(null);
            setCheck({
              kind: "blocked",
              message:
                "The configuration changed while this was saving. The credential is stored; go back a step and return to Review to check the change again.",
            });
            return;
          }
          etag = read.etag;
          next = nextDocument(read.document, draft, editingArg);
        }
      }
      const outcome = await writeDocument(next.doc, { etag, summary: summary(), dryRun: false });
      if (screen.aborted) return;
      if (outcome.kind === "saved") {
        toast.ok(original ? `Saved ${draft.key}.` : `Added ${draft.key}.`);
        data.reload();
        if (draft.method === "device" && original?.method !== "device") {
          onDevice(draft.key);
          return;
        }
        nav.to(["settings", "models", draft.key]);
        // AFTER A SAVE the model's own page has focus (design QA M6); the toast already confirms.
        focusWhenShown(pageHeading);
        return;
      }
      if (outcome.kind === "problems") {
        setEngineProblems([...outcome.problems]);
        setCheck({ kind: "problems" });
      } else if (outcome.kind !== "valid") {
        setPrepared(null);
        setCheck({ kind: "blocked", message: refusalWords(outcome, "save") });
      }
    } finally {
      if (!screen.aborted) setSaving(false);
    }
  }

  const go = (id: WizardStep) => {
    // REVIEW LISTS EVERYTHING, so reaching it is acting on every step.
    if (id === "review") setAttempted(new Set(steps));
    setStep(id);
    if (steps.indexOf(id) > steps.indexOf(reached)) setReached(id);
  };
  const at = steps.indexOf(step);
  const next = steps[at + 1];
  const back = steps[at - 1];

  /** On to the next step when this one is complete; otherwise its problems, and focus on the first. */
  function advance() {
    if (step === "review") {
      if (check.kind === "ready") void save();
      else if (check.kind === "idle") void runCheck();
      return;
    }
    if (local.some((p) => stepOf(p, draft.key) === step)) {
      setAttempted((a) => new Set([...a, step]));
      focusWhenShown(() => firstInvalid(root.current));
      return;
    }
    if (next) go(next);
  }

  /** Close, and give focus back to what opened the wizard (design QA M6). */
  const close = () => {
    onClose();
    focusWhenShown(() => findOpener(opener));
  };
  const dirty = JSON.stringify(draft) !== JSON.stringify(initial);
  const cancel = () => (dirty ? setDiscarding(true) : close());

  /**
   * ENTER MOVES ON from a single-line field, a focused choice, or the step's
   * heading, which takes focus as each step opens. Shift+Enter, a multi-line
   * field, a button and an open list keep Enter for themselves.
   */
  function onKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    const el = event.target as HTMLElement;
    if (el instanceof HTMLTextAreaElement || el instanceof HTMLButtonElement) return;
    if (el.closest("[role='listbox'], [role='menu'], [aria-expanded='true'], [role='dialog']"))
      return;
    if (el instanceof HTMLInputElement || /^H[1-6]$/.test(el.tagName)) {
      event.preventDefault();
      advance();
    }
  }

  const attention = new Set(shown.map((p) => stepOf(p, draft.key)));
  const stepList: StepperStep[] = steps.map((id) => ({
    id,
    label: STEP_LABELS[id],
    ...(attention.has(id) && id !== step ? { status: "attention" as const } : {}),
  }));

  const passed = test.kind === "passed" && test.for === signature;
  const testRow = (
    <span className="models-test-row">
      <span>{storedFor === signature ? "Passed" : testWords(test, signature)}</span>
      {!passed && test.kind !== "running" && storedFor !== signature ? (
        <button type="button" className="t-link" onClick={() => void runTest()}>
          Run test call
        </button>
      ) : null}
    </span>
  );

  const reviewing = step === "review";
  const checking = check.kind === "checking";
  const primary = next ? (
    <Button variant="primary" onClick={advance}>
      Continue
    </Button>
  ) : (
    <Button
      variant="primary"
      onClick={advance}
      loading={saving || checking}
      disabled={listedProblems.length > 0 || check.kind === "blocked"}
    >
      {checking ? "Checking…" : original ? "Save changes" : "Create model"}
    </Button>
  );
  const footer = (
    <>
      {/* BACK IS ALWAYS IN THE ROW, disabled on the first step, so the row keeps one shape. */}
      <Button variant="secondary" disabled={!back} onClick={() => back && go(back)}>
        Back
      </Button>
      {original && !reviewing ? (
        <Button variant="secondary" onClick={() => go("review")}>
          Review changes
        </Button>
      ) : null}
    </>
  );
  const reviewErrors = errorWhere(
    (s) => s[0] === "assistant" || s[0] === "roles" || s[0] === "units" || field("overrides")(s),
  );

  return (
    // The wizard's keyboard: Enter moves on (see onKeyDown). Laid out as if absent.
    <div className="models-keys" ref={root} onKeyDown={onKeyDown}>
      <SetupWizard
        title={original ? `Edit ${draft.key}` : "Add a model"}
        footerStart={
          <Button variant="ghost" onClick={cancel}>
            Cancel
          </Button>
        }
        steps={stepList}
        current={step}
        reached={reached}
        onSelect={(id) => go(id as WizardStep)}
        stepsLabel={original ? `Steps to edit ${draft.key}` : "Steps to add a model"}
        heading={STEP_LABELS[step]}
        lead={LEADS[step]}
        wide={step === "harness" || step === "runs" || step === "assign" || reviewing}
        footer={footer}
        primaryAction={primary}
      >
        <div className="models-stack">
          {step === "harness" ? (
            <HarnessStep
              draft={draft}
              editing={!!original}
              error={errorWhere((s) => s[0] === "key" || field("harness")(s))}
              onName={(key) => set({ key })}
              onHarness={changeHarness}
            />
          ) : null}
          {step === "signin" ? (
            <SignInStep
              draft={draft}
              editing={editing ?? null}
              keptCredential={kept.credential}
              replacing={replacing}
              shared={editing ? sharedSecret(data.models, editing.key, draft.variable) : []}
              storedCloud={kept.cloud}
              errors={{
                credential: pastesCredential(draft)
                  ? errorWhere((s) => s[0] === "values")
                  : undefined,
                variable: errorWhere((s) => s[0] === "variable"),
                env: errorWhere(
                  (s) =>
                    (draft.method === "cloud" && s[0] === "values") ||
                    field("env")(s) ||
                    field("auth")(s),
                ),
              }}
              onChange={(patch) => set(patch)}
              onVariable={(variable) => {
                setVariableTouched(true);
                set({ variable });
              }}
              onReplace={(on) => {
                setReplacing(on);
                set({
                  credential: "",
                  ...(on || !original ? {} : { variable: original.variable }),
                });
              }}
              onChecked={data.reload}
            />
          ) : null}
          {step === "model" ? (
            <ModelStep
              draft={draft}
              error={errorWhere(field("model"))}
              onChange={(model) => set({ model })}
            />
          ) : null}
          {step === "runs" ? (
            <WhereItRuns
              draft={draft}
              error={errorWhere(field("run_in"))}
              onChange={(runIn) => set({ runIn })}
            />
          ) : null}
          {step === "assign" ? (
            <UseItFor
              draft={draft}
              data={data}
              served={original?.uses ?? null}
              error={reviewErrors}
              onChange={(patch) => set(patch)}
            />
          ) : null}
          {step === "limits" ? (
            <LimitsStep
              draft={draft}
              errors={{
                concurrent: errorWhere(field("max_concurrent")),
                timeout: errorWhere(field("timeout_seconds")),
              }}
              onChange={(patch) => set(patch)}
            />
          ) : null}
          {step === "test" ? (
            <TestStep
              draft={draft}
              test={test}
              current={signature}
              editing={!!original}
              signIn={signIn}
              onRun={() => void runTest()}
            />
          ) : null}
          {reviewing ? (
            <Review
              draft={draft}
              data={data}
              before={prepared?.before ?? data.doc}
              after={
                prepared?.next.doc ??
                (data.doc ? nextDocument(data.doc, draft, editingArg).doc : null)
              }
              dropped={
                (prepared?.next ?? (data.doc ? nextDocument(data.doc, draft, editingArg) : null))
                  ?.dropped ?? []
              }
              editing={!!original}
              served={original?.uses ?? noUses()}
              keptCredential={kept.credential}
              problems={check.kind === "checking" ? [] : listedProblems}
              check={check}
              testRow={testRow}
              onGo={(id) => go(id)}
            />
          ) : null}
        </div>
      </SetupWizard>
      <ConfirmModal
        open={discarding}
        onClose={() => setDiscarding(false)}
        onConfirm={() => {
          setDiscarding(false);
          close();
        }}
        title={original ? "Discard these changes?" : "Discard this model?"}
        message="Nothing has been saved."
        confirmLabel="Discard"
        cancelLabel="Keep editing"
        destructive
      />
    </div>
  );
}
