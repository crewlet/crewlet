/**
 * Add a model, or edit one, step by step: its name and harness, how it signs in,
 * the model, where it runs a seat's executor and its limits, then Review,
 * where the test call is run and said in one line. Finish
 * and save runs the test call and the engine's check before anything is
 * stored, and the engine's problems land on the fields they are about, each
 * with the step that fixes it.
 *
 * FINISH AND SAVE AS SOON AS IT CAN SAVE: beside Continue from the first step
 * where the draft has nothing missing, so the optional steps left keep their
 * defaults; on Review it is the one primary action.
 *
 * EDITING IS THE SAME WIZARD, prefilled from the model (`editing`): every step
 * is reachable at once, the name and the harness are its identity and stay, a
 * stored credential is shown as stored and replaced only on request, Finish and
 * save is a footer button on every step, and Review lists every seat it moves.
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

import { useContext, useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { Button, SetupWizard, useToast, type StepperStep } from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import { readOnlyFrom, useAssistantDraft, useDraftState } from "~/app/assistant/draft.ts";
import { useNavigator, useUnloadGuard } from "~/app/router.tsx";
import { nodeStanding } from "~/lib/nodeStanding.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { listed } from "~/lib/format.ts";
import { intentOf, usesForIntent, withCategory, noUses } from "~/lib/modelUses.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import {
  findOpener,
  firstInvalid,
  focusWhenShown,
  openerNow,
  pageHeading,
} from "~/lib/wizardFocus.ts";
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
import { storeCredential, testCredential, writeDocument, type ModelAnswer } from "../api.ts";
import { entityOf as fleetEntity, saveFleet } from "../fleets/data.ts";
import type { ModelsData, ModelView } from "../data.ts";
import { failureWords, type SignInFacts } from "../flows/TestDialog.tsx";
import type { WizardAdd, WizardAt, WizardStep } from "../parts.tsx";
import { useCheckModel } from "../useCheckModel.ts";
import { CATEGORY_WORDS } from "../words.ts";
import {
  methodsOf,
  providersFor,
  STEP_LABELS,
  baseOf,
  canExecute,
  credentialScope,
  credentialValues,
  distinctProblems,
  draftOf,
  draftProblems,
  emptyDraft,
  entityOf,
  messageOf,
  nextDocument,
  pastesCredential,
  fixable,
  stepOf,
  stepsFor,
  hasSandbox,
  keptOf,
  withHostSandbox,
  suggestedVariable,
  withChoices,
  type Harness,
  type Draft,
  type NextDocument,
} from "./draft.ts";
import { MODEL_DRAFT_HELP, modelDraftFields } from "./assistantDraft.ts";
import { HarnessStep } from "./steps/HarnessStep.tsx";
import { LimitsStep } from "./steps/LimitsStep.tsx";
import { ModelStep } from "./steps/ModelStep.tsx";
import { FinishStatus, type Finish } from "./FinishStatus.tsx";
import {
  Review,
  defaultFleetOf,
  fitsFleet,
  type ReviewCheck,
  type SaveStage,
} from "./steps/Review.tsx";
import { SignInStep, sharedSecret } from "./steps/SignInStep.tsx";
import { routeOf, testWords, type TestState } from "./steps/testCall.ts";
import { WhereItRuns } from "./steps/WhereItRuns.tsx";
import { everythingHelpers } from "./uses.ts";

/** A change the engine's dry run accepted, against the revision it read. */
interface Prepared {
  etag: string;
  before: CompanyDocument;
  next: NextDocument;
}

const LEADS: Record<WizardStep, string> = {
  harness:
    "Name the model and choose the harness it runs. Seats and Ask Crewlet name it, so keep it short.",
  signin: "How this model proves who it is to the vendor.",
  model: "The model the harness runs, in its own naming.",
  runs: "Where a seat's executor runs when it launches on this model.",
  limits:
    "How many harness processes run at once, how long one may go without progress, and its web tools.",
  review: "Everything this saves. The engine checks it first.",
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
    editing ? draftOf(editing, data.executors) : newDraft(data),
    target?.add ?? [],
    new Set(data.seats.map((s) => s.handle)),
  );
}

/** A new model's draft: a variable no other model reads, and for the company's first model, everything. */
function newDraft(data: ModelsData): Draft {
  const d = emptyDraft(data.executors);
  const taken = data.models.map((m) => m.admin?.sign_in.variable ?? "").filter(Boolean);
  d.variable = suggestedVariable(d, taken);
  // THE COMPANY'S FIRST MODEL does everything for every seat and helper until a second one arrives.
  if (data.models.length === 0 && data.seats.length > 0) {
    const helpers = everythingHelpers(data, d.harness);
    d.intent = "all";
    d.uses = usesForIntent(
      "all",
      data.seats.map((s) => s.handle),
      helpers.assistant,
      helpers.advisor,
    );
  }
  return d;
}

/**
 * The draft with the uses an action asked for added (the seats moved off a
 * limit, the assistant), for Review to list and save.
 *
 * ONLY WHAT THE COMPANY HAS NOW: a seat it no longer has (a link kept from
 * before a seat was removed, or typed by hand) is dropped without a word, and
 * so is coding on a harness that runs none. Review lists every move before a write.
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
  const checkModel = useCheckModel();
  // THE CONTROL THAT OPENED THE WIZARD is read while its page is still on screen.
  const [opener] = useState(openerNow);
  const [original] = useState(() => (editing ? draftOf(editing, data.executors) : null));
  const [base] = useState(() => (editing ? baseOf(editing) : undefined));
  const [initial] = useState(() => openingDraft(data, editing, target));
  const [draft, setDraft] = useDraftState<Draft>(initial);
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
    write: readOnlyFrom(setDraft),
  });
  const [reached, setReached] = useState<WizardStep>(editing ? "review" : step);
  const [attempted, setAttempted] = useState<ReadonlySet<WizardStep>>(new Set());
  // A RENEW OR A REPLACE opens the stored secret for a new value at once.
  const [replacing, setReplacing] = useState(
    !!original && target?.focus === "credential" && pastesCredential(original),
  );
  /** Whether the secret's name is the reader's or the edited model's, rather than suggested. */
  const [variableNamed, setVariableNamed] = useState(!!editing);
  const [test, setTest] = useState<TestState>({ kind: "idle" });
  const [engineProblems, setEngineProblems] = useState<ConfigProblem[]>([]);
  const [check, setCheck] = useState<ReviewCheck>({ kind: "idle" });
  const [prepared, setPrepared] = useState<Prepared | null>(null);
  const [finish, setFinish] = useState<Finish>({ kind: "idle" });
  const finishing = useRef(false);
  const [saving, setSaving] = useState<SaveStage | false>(false);
  /** The draft whose credential is stored already, so a retried save does not store it twice. */
  const [storedFor, setStoredFor] = useState<string | null>(null);
  const [discarding, setDiscarding] = useState(false);
  /** Whether an added model joins the default fleet: null until the reader chooses, which reads as yes. */
  const [toDefault, setToDefault] = useState<boolean | null>(null);
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
  const kept = keptOf(draft, original, base, replacing);
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
  const local = draftProblems(draft, existing, kept, !!original);
  const shown = [...local.filter((p) => attempted.has(stepOf(p, draft.key))), ...engineProblems];
  const listedProblems = distinctProblems(local, engineProblems, draft.key);
  const errorWhere = (match: (s: (string | number)[]) => boolean) => {
    const p = shown.find((x) => match(x.segments ?? []));
    return p ? messageOf(p) : undefined;
  };
  const field = (name: string) => (s: (string | number)[]) =>
    s[0] === "providers" && s[1] === "llm" && s[2] === draft.key && s[3] === name;

  const set = (patch: Partial<Draft>) => {
    // A CHANGED CHOICE CLEARS WHAT BELONGS TO THE OLD ONE (`withChoices`): the
    // model, its effort, the pasted credential and the secret's name.
    const next = withChoices(draft, patch, {
      original,
      taken,
      named: variableNamed,
      everything: (d) => everythingHelpers(data, d.harness),
    });
    setDraft(next.draft);
    setVariableNamed(next.named);
    // THE STORED CREDENTIAL IS ONLY ITS OWN SCOPE'S, so replacing it ends where the scope moved.
    if (original && credentialScope(next.draft) !== credentialScope(original)) setReplacing(false);
    setEngineProblems([]);
    setCheck({ kind: "idle" });
    setPrepared(null);
    // A change mid-check abandons the check, so the save never stores the draft as it was.
    if (finishing.current) inflight.current?.abort();
    setFinish({ kind: "idle" });
  };

  const changeHarness = (harness: Harness) => {
    // BACK ON THE EDITED MODEL'S HARNESS, its own sign-in comes back with it.
    if (original && harness === original.harness) {
      set({ harness, method: original.method, provider: original.provider, cloud: original.cloud });
      return;
    }
    const providers = providersFor(harness);
    set({
      harness,
      method: methodsOf(harness)[0] ?? "unchanged",
      ...(providers.length > 0 && !providers.some((p) => p.value === draft.provider)
        ? { provider: providers[0]!.value }
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

  // AN ADDED MODEL JOINS THE DEFAULT FLEET where it fits it, unless the reader unticks it.
  const home = original ? null : defaultFleetOf(data);
  const joining = !!home && fitsFleet(home, draft) && (toDefault ?? true);

  /**
   * Adds the saved model to the default fleet: after the model, since the
   * engine checks a member against the models. Why it was not added, or "".
   */
  async function joinDefault(): Promise<string> {
    if (!home || !joining) return "";
    const members = [...home.members.map((m) => m.entry), draft.key];
    const outcome = await saveFleet(
      home.key,
      fleetEntity(home, members),
      `Add ${draft.key} to the ${home.name} fleet`,
    );
    if (outcome.kind === "saved" || outcome.kind === "unchanged") return "";
    const why =
      outcome.kind === "problems"
        ? outcome.problems.map((p) => messageOf(p)).join(" ")
        : outcome.kind === "valid"
          ? ""
          : refusalWords(outcome, "save");
    return `${draft.key} is added, but not to ${home.name}${why ? `: ${why}` : "."}`;
  }

  // THIS NODE'S HEALTH, read as it stands when the host's write is waited on.
  const client = useContext(ClientContext);
  // A FIRST CODING MODEL WITH NO PLACE TO RUN IT: the save sets up this host (M-002).
  const needsHost = canExecute(draft) && !!data.doc && !hasSandbox(data.doc);

  /**
   * Sets up this engine's host to run coding, checked then written, before a
   * first coding model's test: the engine's own remedy for a company with no
   * `providers.sandbox`. Whether it stands now.
   */
  async function setUpHost(signal: AbortSignal): Promise<boolean> {
    setCheck({ kind: "checking", what: "document" });
    const read = await getConfig(signal);
    if (read.kind !== "document") {
      setCheck({ kind: "blocked", message: "No company is configured yet. Set one up first." });
      return false;
    }
    if (hasSandbox(read.document)) return true;
    const doc = withHostSandbox(read.document);
    const dry = await writeDocument(doc, { etag: read.etag, summary: "", dryRun: true }, signal);
    if (dry.kind !== "valid") {
      setCheck({ kind: "blocked", message: refusalWords(dry as ConfigRefusal, "check") });
      return false;
    }
    const saved = await writeDocument(doc, {
      etag: read.etag,
      summary: "Set up running coding on this engine's host",
      dryRun: false,
    });
    if (saved.kind !== "saved") {
      setCheck({ kind: "blocked", message: refusalWords(saved as ConfigRefusal, "save") });
      return false;
    }
    // THE TEST RUNS ON THIS NODE ONCE IT RUNS THE HOST: tested the moment the
    // write returned, the node had not yet built the box the test launches in.
    const write = { revisionId: saved.revisionId, epoch: saved.epoch };
    const ran = await until(() => {
      const h = client?.store.state.health ?? null;
      // NO HEALTH READ YET: nothing to wait on, so the test goes ahead.
      return h ? nodeStanding(h, write).kind : "applied";
    }, signal);
    if (ran === "refused") {
      setCheck({
        kind: "blocked",
        message: "This engine did not take up running coding on its host. Nothing else was saved.",
      });
      return false;
    }
    return true;
  }

  /**
   * Review's check: the test call where a credential is pasted (or `withTest`
   * asks for one), then the whole next document's dry run. The prepared write, or null.
   */
  async function runCheck(withTest = false): Promise<Prepared | null> {
    inflight.current?.abort();
    const ctrl = new AbortController();
    inflight.current = ctrl;
    try {
      if (local.length > 0) {
        setCheck({ kind: "problems" });
        return null;
      }
      if (
        (withTest || names.length > 0) &&
        storedFor !== signature &&
        !(test.kind === "passed" && test.for === signature) &&
        // NO PLACE TO RUN CODING YET: the engine checks a draft's test against
        // the company as it stands, which refuses it, so the test waits for
        // Finish and save to set up this host first; opening Review writes nothing.
        (!needsHost || finishing.current)
      ) {
        if (needsHost && !(await setUpHost(ctrl.signal))) return null;
        setCheck({ kind: "checking", what: "credential" });
        if (!(await runTest(ctrl.signal))) {
          setCheck({ kind: "problems" });
          return null;
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
        return null;
      }
      const next = nextDocument(read.document, draft, editingArg);
      const [outcome, baseline] = await Promise.all([
        writeDocument(next.doc, { etag: read.etag, summary: "", dryRun: true }, ctrl.signal),
        writeDocument(read.document, { etag: read.etag, summary: "", dryRun: true }, ctrl.signal),
      ]);
      if (outcome.kind === "valid") {
        const ready = { etag: read.etag, before: read.document, next };
        setPrepared(ready);
        setCheck({
          kind: "ready",
          warnings: introducedWarnings(
            baseline.kind === "valid" ? baseline.warnings : [],
            outcome.warnings,
          ),
        });
        return ready;
      } else if (outcome.kind === "problems") {
        setEngineProblems([...outcome.problems]);
        setCheck({ kind: "problems" });
      } else if (outcome.kind !== "saved") {
        setCheck({ kind: "blocked", message: refusalWords(outcome, "check") });
      }
    } catch (err) {
      if (!isAbort(err)) throw err;
    }
    return null;
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
    // EVERYTHING WITH NO SEAT is the helpers it gave, by name.
    const helpers = [
      ...(draft.uses.assistant ? ["Ask Crewlet"] : []),
      ...(draft.uses.advisor ? ["the Advisor"] : []),
    ];
    const what =
      draft.intent === "all"
        ? seats > 0
          ? "every job"
          : listed(helpers)
        : draft.intent === "none" || draft.intent === "custom"
          ? ""
          : draft.intent === "assistant"
            ? "Ask Crewlet"
            : draft.intent === "advisor"
              ? "the Advisor"
              : CATEGORY_WORDS[draft.intent].label.toLowerCase();
    if (!what) return verb;
    return `${verb} for ${what}${seats > 0 ? ` (${seats} ${seats === 1 ? "seat" : "seats"})` : ""}`;
  }

  /** Stores the checked change (Review's, or `ready` as Finish and save checked it); whether it saved. */
  async function save(ready?: Prepared): Promise<boolean> {
    const checked = ready ?? (check.kind === "ready" ? prepared : null);
    if (!checked) return false;
    // THE SAVE RUNS TO ITS END ONCE STARTED, whatever happens to the wizard:
    // its requests take no signal, since stopped between the credential and
    // the document it would leave a sealed secret no document names. What
    // the wizard does with each answer (its state, the toast, the move to the
    // model's page) is what its lifetime gates: a save that answers after the
    // person left acts for nothing on screen.
    const screen = lifetime();
    const storing = names.length > 0 && storedFor !== signature;
    setSaving(storing ? "credential" : "document");
    try {
      let { etag, next } = checked;
      if (storing) {
        const draftEntity = entityChanged ? { entity } : {};
        const stored = await storeCredential(
          draft.key,
          { values, ...draftEntity, createOnly },
          etag,
        );
        if (stored.kind === "refused") {
          if (screen.aborted) return false;
          const refusal = credentialRefusal(stored, names);
          setEngineProblems(refusal.problems.length > 0 ? refusal.problems : []);
          setCheck(
            refusal.problems.length > 0
              ? { kind: "problems" }
              : { kind: "blocked", message: refusal.message },
          );
          return false;
        }
        if (!screen.aborted) setStoredFor(signature);
        // A ROTATION RE-ACTIVATES THE REVISION that reads it, which moves the
        // tag: the document is read again, and written on only if nothing
        // but that re-activation happened in between.
        if (stored.value.reload) {
          const read = await getConfig();
          if (
            read.kind !== "document" ||
            JSON.stringify(read.document) !== JSON.stringify(checked.before)
          ) {
            if (screen.aborted) return false;
            setPrepared(null);
            setCheck({
              kind: "blocked",
              message:
                "The configuration changed while this was saving. The credential is stored; go back a step and return to Review to check the change again.",
            });
            return false;
          }
          etag = read.etag;
          next = nextDocument(read.document, draft, editingArg);
        }
      }
      if (!screen.aborted) setSaving("document");
      const outcome = await writeDocument(next.doc, { etag, summary: summary(), dryRun: false });
      const unjoined = outcome.kind === "saved" ? await joinDefault() : "";
      if (screen.aborted) {
        if (unjoined) toast.failed(unjoined);
        return outcome.kind === "saved";
      }
      if (outcome.kind === "saved") {
        toast.ok(original ? `Saved ${draft.key}.` : `Added ${draft.key}.`);
        if (unjoined) toast.failed(unjoined);
        data.reload();
        if (draft.method === "device" && original?.method !== "device") {
          onDevice(draft.key);
          return true;
        }
        if (original) {
          // AN EDIT RETURNS TO WHERE IT WAS OPENED, as Cancel does: the
          // list's row menu back to the list (on the lens it was on), the
          // model's page back to that page. Sending every save to the
          // model's page took a person who renewed a token from the list
          // away from the list they were working down. The toast confirms,
          // and focus goes back to the control that opened the wizard.
          close();
          return true;
        }
        // A NEW MODEL has no place it was opened from but the Add button:
        // its own page is where it is first seen, and takes focus (design QA M6).
        nav.to(["settings", "models", draft.key], { added: draft.key });
        focusWhenShown(pageHeading);
        return true;
      }
      if (outcome.kind === "problems") {
        setEngineProblems([...outcome.problems]);
        setCheck({ kind: "problems" });
      } else if (outcome.kind !== "valid") {
        setPrepared(null);
        setCheck({ kind: "blocked", message: refusalWords(outcome, "save") });
      }
      return false;
    } finally {
      if (!screen.aborted) setSaving(false);
    }
  }

  /** One click from any step of an edit: the test call, the engine's check, then the save. */
  async function finishAndSave() {
    if (finishing.current || saving !== false) return;
    finishing.current = true;
    setFinish({ kind: "running" });
    try {
      if (local.length > 0) setAttempted(new Set(steps));
      // A new device sign-in is made after the save, so it has nothing to test yet.
      const device = draft.method === "device" && original?.method !== "device";
      // ON REVIEW A CHECKED CHANGE IS SAVED AS CHECKED: an edit as Review
      // always saved it, an add once its test call passed on this draft.
      const tested = test.kind === "passed" && test.for === signature;
      const checked =
        step === "review" && check.kind === "ready" && prepared && (original || device || tested)
          ? prepared
          : null;
      const ready = checked ?? (await runCheck(!device));
      if (!ready && inflight.current?.signal.aborted) return;
      const saved = ready ? await save(ready) : false;
      if (!saved && !lifetime().aborted) setFinish({ kind: "failed" });
    } finally {
      finishing.current = false;
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
      void finishAndSave();
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
  // A RELOAD OR A CLOSED TAB ASKS FIRST while something is typed: the draft
  // lives here alone, and a reload on its second step threw it away unasked.
  useUnloadGuard(dirty);
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

  // A STEP IS FLAGGED ONLY FOR WHAT IT CAN FIX: a program missing on the host is none's.
  const attention = new Set(shown.filter(fixable).map((p) => stepOf(p, draft.key)));
  const stepList: StepperStep[] = steps.map((id) => ({
    id,
    label: STEP_LABELS[id],
    ...(attention.has(id) && id !== step ? { status: "attention" as const } : {}),
  }));

  // A NEW DEVICE SIGN-IN is made after the save, so it has nothing to test yet.
  const signsInAfter = !original && draft.method === "device";
  const passed = storedFor === signature || (test.kind === "passed" && test.for === signature);
  const testing = test.kind === "running" && test.for === signature;
  // THE TEST CALL'S BUTTON AND ITS RESULT, one line on Review. It costs a few tokens.
  const testRow = signsInAfter ? (
    "After it is saved"
  ) : needsHost && !passed && !testing ? (
    // THE TEST NEEDS THE HOST SET UP, which Finish and save does first.
    "Runs when you save"
  ) : (
    <span className="models-test-row">
      {/* ONE LINE, the whole of what the engine said on hover. */}
      <span {...(test.kind === "failed" && test.for === signature ? { title: test.message } : {})}>
        {storedFor === signature ? "Passed" : testWords(test, signature)}
      </span>
      {!passed ? (
        <Button
          size="small"
          variant="secondary"
          loading={testing}
          disabled={testing || !draft.key}
          onClick={() => void runTest()}
        >
          {test.kind === "failed" && test.for === signature ? "Test again" : "Run test call"}
        </Button>
      ) : null}
    </span>
  );

  const reviewing = step === "review";
  // REVIEW'S PROBLEMS BLOCK ITS SAVE until a step changes the draft: pressing
  // it again only asked the engine the same question.
  const blocked = reviewing && check.kind === "problems" && listedProblems.length > 0;
  const finishButton = (variant: "primary" | "secondary") => (
    <Button
      variant={variant}
      onClick={() => void finishAndSave()}
      loading={finish.kind === "running"}
      disabled={finish.kind === "running" || saving !== false || blocked}
    >
      Finish and save
    </Button>
  );
  const primary = next ? (
    <Button variant="primary" onClick={advance}>
      Continue
    </Button>
  ) : (
    finishButton("primary")
  );
  const footer = (
    <>
      {/* BACK IS ALWAYS IN THE ROW, disabled on the first step, so the row keeps one shape. */}
      <Button variant="secondary" disabled={!back} onClick={() => back && go(back)}>
        Back
      </Button>
      {/* AN EDIT CAN SAVE FROM ANY STEP; an add once nothing it needs is missing. */}
      {!reviewing && (original || local.length === 0) ? finishButton("secondary") : null}
    </>
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
        wide={step === "harness" || step === "runs" || reviewing}
        footer={footer}
        primaryAction={primary}
      >
        <div className="models-stack">
          {!reviewing ? (
            <FinishStatus
              finish={finish}
              test={test}
              check={check}
              saving={saving}
              problems={listedProblems}
              modelKey={draft.key}
              secretNoun={draft.method === "token" ? "token" : "key"}
              pasted={names.length > 0}
              editing={!!original}
              onGo={go}
            />
          ) : null}
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
              onVariable={(variable) => set({ variable })}
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
              effortError={errorWhere(field("effort"))}
              onChange={(model) => set({ model })}
              onEffort={(effort) => set({ effort })}
            />
          ) : null}
          {step === "runs" ? (
            <WhereItRuns
              draft={draft}
              error={errorWhere(field("run_in"))}
              onChange={(runIn) => set({ runIn })}
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
          {reviewing ? (
            <Review
              draft={draft}
              data={data}
              dropped={
                (prepared?.next ?? (data.doc ? nextDocument(data.doc, draft, editingArg) : null))
                  ?.dropped ?? []
              }
              editing={!!original}
              served={original?.uses ?? noUses()}
              keptCredential={kept.credential}
              problems={check.kind === "checking" ? [] : listedProblems}
              check={check}
              saving={saving}
              testRow={testRow}
              toDefault={toDefault}
              onToDefault={setToDefault}
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

/**
 * Waits for this node to conclude a revision: "applied" or "passed" once it
 * runs it or a later one, "refused" where it did not, or "applying" when 30
 * seconds pass with neither, which the caller goes on from.
 */
async function until(
  standing: () => "applied" | "passed" | "refused" | "applying",
  signal: AbortSignal,
): Promise<"applied" | "passed" | "refused" | "applying"> {
  const deadline = Date.now() + 30_000;
  for (;;) {
    const now = standing();
    if (now !== "applying" || Date.now() > deadline || signal.aborted) return now;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
}
